package auth

import (
	"agora/shared/csrf"
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost controls the work factor for password hashing. 12 is a
// reasonable default; raise it as hardware gets faster, but re-test login
// latency when you do.
const bcryptCost = 12

// dummyHash is compared against on every "user not found" login, so that
// path takes about as long as a "wrong password" login. Without this, an
// attacker can time login responses to enumerate which emails are
// registered.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password-used-for-timing"), bcryptCost)

type Service struct {
	DB *store.DB

	loginLimiter *loginRateLimiter
}

func New(db *store.DB) *Service {
	return &Service{
		DB:           db,
		loginLimiter: newLoginRateLimiter(5, 15*time.Minute),
	}
}

// HashForSeeding exposes the password hasher for seed/demo-account
// creation in main.go, so there's exactly one hashing implementation in
// the codebase instead of a second copy living next to main().
func HashForSeeding(password string) (string, error) {
	return hashPassword(password)
}

// ── Password hashing ────────────────────────────────────

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(hash), nil
}

func checkPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// validatePassword enforces a length policy. bcrypt silently truncates
// input over 72 bytes; we reject it explicitly instead of letting that
// truncation happen invisibly.
func validatePassword(password string) error {
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if len(password) > 72 {
		return fmt.Errorf("password must be at most 72 characters")
	}
	return nil
}

func validateEmail(email string) error {
	if len(email) > 254 {
		return fmt.Errorf("email is too long")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return fmt.Errorf("invalid email address")
	}
	return nil
}

// ── Legacy (pre-bcrypt) hash support ───────────────────
//
// The original scheme was SHA-256 of a fixed string constant + password
// (no per-user salt). This block exists only to let accounts created
// under that scheme log in once and get transparently migrated to
// bcrypt — delete it once you've confirmed no rows remain with a
// 64-char legacy hash:
//
//	SELECT count(*) FROM users WHERE length(password_hash) = 64;

var legacySalts = []string{"agora_salt_2024_", "agora_salt_2026_"}

func legacyHash(password, salt string) string {
	h := sha256.New()
	h.Write([]byte(salt + password))
	return hex.EncodeToString(h.Sum(nil))
}

func isLegacyHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func checkLegacyPassword(hash, password string) bool {
	for _, salt := range legacySalts {
		candidate := legacyHash(password, salt)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(hash)) == 1 {
			return true
		}
	}
	return false
}

// ── Session IDs ──────────────────────────────────────────

func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// Never fall back to a weak/zero session ID if we can't get
		// entropy — fail the login instead.
		return "", fmt.Errorf("generating session id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ── Registration ────────────────────────────────────────

func (s *Service) Register(name, email, password, role string) (*models.User, error) {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(strings.ToLower(email))

	if name == "" || email == "" || password == "" {
		return nil, fmt.Errorf("all fields required")
	}
	if len(name) > 200 {
		return nil, fmt.Errorf("name is too long")
	}
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if err := validatePassword(password); err != nil {
		return nil, err
	}

	// Only two roles are ever accepted from user input; anything else
	// (including someone POSTing role=admin) silently falls back to the
	// least-privileged role. This function must remain the *only* place
	// a user's own role is set from a request — nothing downstream should
	// ever re-read r.FormValue("role") to decide what a user is allowed
	// to do.
	if role != "seller" {
		role = "buyer"
	}

	hash, err := hashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("could not create account")
	}

	user, err := s.DB.CreateUser(models.User{
		Name:         name,
		Email:        email,
		PasswordHash: hash,
		Role:         role,
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

// ── Login ────────────────────────────────────────────────

func (s *Service) Login(email, password, limiterKey string) (*models.User, string, error) {
	email = strings.TrimSpace(strings.ToLower(email))

	if !s.loginLimiter.Allow(limiterKey) {
		return nil, "", fmt.Errorf("too many login attempts, please try again later")
	}

	user, err := s.DB.GetUserByEmail(email)
	if err != nil {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password)) // timing parity, see dummyHash
		s.loginLimiter.RecordFailure(limiterKey)
		return nil, "", fmt.Errorf("invalid email or password")
	}

	ok := checkPassword(user.PasswordHash, password)
	if !ok && isLegacyHash(user.PasswordHash) && checkLegacyPassword(user.PasswordHash, password) {
		ok = true
		if newHash, err := hashPassword(password); err == nil {
			s.DB.UpdateUserPasswordHash(user.ID, newHash) // best-effort upgrade
		}
	}
	if !ok {
		s.loginLimiter.RecordFailure(limiterKey)
		return nil, "", fmt.Errorf("invalid email or password")
	}
	s.loginLimiter.RecordSuccess(limiterKey)

	sessionID, err := generateSessionID()
	if err != nil {
		return nil, "", fmt.Errorf("could not start session")
	}
	if err := s.DB.CreateSession(models.Session{
		ID:        sessionID,
		UserID:    user.ID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}); err != nil {
		return nil, "", fmt.Errorf("could not start session")
	}
	return user, sessionID, nil
}

func clientIP(r *http.Request) string {
	// If you're behind a trusted reverse proxy, read X-Forwarded-For here
	// instead (and strip it upstream so clients can't spoof it).
	return r.RemoteAddr
}

// ── Routes ───────────────────────────────────────────────

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			token, err := csrf.IssueCookie(w, r)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			tmpl.ExecuteTemplate(w, "register.html", map[string]interface{}{
				"Title": "Register", "CSRFToken": token,
			})
		case http.MethodPost:
			r.ParseForm()
			if err := csrf.Verify(r); err != nil {
				http.Error(w, "invalid request, please reload the page", http.StatusForbidden)
				return
			}
			_, err := s.Register(r.FormValue("name"), r.FormValue("email"), r.FormValue("password"), r.FormValue("role"))
			if err != nil {
				token, _ := csrf.IssueCookie(w, r)
				tmpl.ExecuteTemplate(w, "register.html", map[string]interface{}{
					"Title": "Register", "Error": err.Error(), "CSRFToken": token,
				})
				return
			}
			http.Redirect(w, r, "/login?registered=1", http.StatusFound)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			msg := ""
			if r.URL.Query().Get("registered") == "1" {
				msg = "Registration successful! Please log in."
			}
			token, err := csrf.IssueCookie(w, r)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			tmpl.ExecuteTemplate(w, "login.html", map[string]interface{}{
				"Title": "Login", "Message": msg, "CSRFToken": token,
			})
		case http.MethodPost:
			r.ParseForm()
			if err := csrf.Verify(r); err != nil {
				http.Error(w, "invalid request, please reload the page", http.StatusForbidden)
				return
			}
			key := strings.ToLower(strings.TrimSpace(r.FormValue("email"))) + "|" + clientIP(r)
			_, sessionID, err := s.Login(r.FormValue("email"), r.FormValue("password"), key)
			if err != nil {
				token, _ := csrf.IssueCookie(w, r)
				tmpl.ExecuteTemplate(w, "login.html", map[string]interface{}{
					"Title": "Login", "Error": err.Error(), "CSRFToken": token,
				})
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     "session_id",
				Value:    sessionID,
				Path:     "/",
				Expires:  time.Now().Add(24 * time.Hour),
				HttpOnly: true,
				Secure:   csrf.IsTLS(r),
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusFound)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Logout is POST-only: a GET logout link can be triggered cross-site
	// (e.g. an <img> tag on another page), which is a CSRF issue even
	// though the impact is "just" logging someone out.
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.ParseForm()
		if err := csrf.Verify(r); err != nil {
			http.Error(w, "invalid request, please reload the page", http.StatusForbidden)
			return
		}
		if cookie, err := r.Cookie("session_id"); err == nil {
			s.DB.DeleteSession(cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name: "session_id", Value: "", Path: "/", Expires: time.Now().Add(-time.Hour),
			HttpOnly: true, Secure: csrf.IsTLS(r), SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	})

	_ = middleware.GetUserID
}

// ── In-memory login rate limiter ────────────────────────
//
// Blunts credential-stuffing / brute force against /login (OWASP A07:
// Identification & Authentication Failures). This is per-process only —
// if you run more than one instance behind a load balancer, move this to
// a shared store (Redis, or a login_attempts table) so the limit applies
// across all of them, and pair it with rate limiting at the edge/proxy.

type loginRateLimiter struct {
	mu          sync.Mutex
	maxAttempts int
	window      time.Duration
	attempts    map[string][]time.Time
}

func newLoginRateLimiter(maxAttempts int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		maxAttempts: maxAttempts,
		window:      window,
		attempts:    make(map[string][]time.Time),
	}
}

func (l *loginRateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(key)
	return len(l.attempts[key]) < l.maxAttempts
}

func (l *loginRateLimiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts[key] = append(l.attempts[key], time.Now())
}

func (l *loginRateLimiter) RecordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func (l *loginRateLimiter) pruneLocked(key string) {
	cutoff := time.Now().Add(-l.window)
	var kept []time.Time
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.attempts[key] = kept
}
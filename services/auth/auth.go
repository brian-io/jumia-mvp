package auth

import (
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Service struct {
	DB *store.DB
}

func New(db *store.DB) *Service { return &Service{DB: db} }

func hashPassword(password string) string {
	h := sha256.New()
	h.Write([]byte("agora_salt_2024_" + password))
	return hex.EncodeToString(h.Sum(nil))
}

func generateSessionID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) Register(name, email, password, role string) (*models.User, error) {
	if name == "" || email == "" || password == "" {
		return nil, fmt.Errorf("all fields required")
	}
	if len(password) < 6 {
		return nil, fmt.Errorf("password must be at least 6 characters")
	}
	if role != "seller" {
		role = "buyer"
	}
	return s.DB.CreateUser(models.User{
		Name: name, Email: strings.ToLower(email),
		PasswordHash: hashPassword(password), Role: role,
	})
}

func (s *Service) Login(email, password string) (*models.User, string, error) {
	user, err := s.DB.GetUserByEmailAndHash(strings.ToLower(email), hashPassword(password))
	if err != nil {
		return nil, "", fmt.Errorf("invalid email or password")
	}
	sessionID := generateSessionID()
	s.DB.CreateSession(models.Session{
		ID: sessionID, UserID: user.ID,
		ExpiresAt: time.Now().Add(24 * time.Hour),
	})
	return user, sessionID, nil
}

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			tmpl.ExecuteTemplate(w, "register.html", map[string]interface{}{"Title": "Register"})
			return
		}
		r.ParseForm()
		_, err := s.Register(r.FormValue("name"), r.FormValue("email"), r.FormValue("password"), r.FormValue("role"))
		if err != nil {
			tmpl.ExecuteTemplate(w, "register.html", map[string]interface{}{"Title": "Register", "Error": err.Error()})
			return
		}
		http.Redirect(w, r, "/login?registered=1", http.StatusFound)
	})

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			msg := ""
			if r.URL.Query().Get("registered") == "1" {
				msg = "Registration successful! Please log in."
			}
			tmpl.ExecuteTemplate(w, "login.html", map[string]interface{}{"Title": "Login", "Message": msg})
			return
		}
		r.ParseForm()
		_, sessionID, err := s.Login(r.FormValue("email"), r.FormValue("password"))
		if err != nil {
			tmpl.ExecuteTemplate(w, "login.html", map[string]interface{}{"Title": "Login", "Error": err.Error()})
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "session_id", Value: sessionID, Path: "/",
			Expires: time.Now().Add(24 * time.Hour), HttpOnly: true,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session_id"); err == nil {
			s.DB.DeleteSession(cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "session_id", Value: "", Path: "/", Expires: time.Now().Add(-time.Hour)})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	_ = middleware.GetUserID
}

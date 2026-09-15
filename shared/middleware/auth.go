package middleware

import (
	"agora/shared/store"
	"context"
	"net/http"
)

type contextKey string

const UserIDKey contextKey = "userID"
const UserRoleKey contextKey = "userRole"
const UserNameKey contextKey = "userName"

// Auth reads the session cookie, if present, and attaches the user's ID,
// role and name to the request context for downstream handlers. It never
// rejects a request on its own — an absent or invalid session simply
// means the context has no user in it. Routes that require a logged-in
// user should wrap themselves in RequireAuth or RequireRole below.
func Auth(db *store.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie("session_id")
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			_, user, err := db.GetSession(cookie.Value)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := context.WithValue(r.Context(), UserIDKey, user.ID)
			ctx = context.WithValue(ctx, UserRoleKey, user.Role)
			ctx = context.WithValue(ctx, UserNameKey, user.Name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetUserID(r *http.Request) (int, bool) {
	v := r.Context().Value(UserIDKey)
	if v == nil {
		return 0, false
	}
	return v.(int), true
}

func GetUserRole(r *http.Request) string {
	v := r.Context().Value(UserRoleKey)
	if v == nil {
		return ""
	}
	return v.(string)
}

func GetUserName(r *http.Request) string {
	v := r.Context().Value(UserNameKey)
	if v == nil {
		return ""
	}
	return v.(string)
}

// RequireAuth guards routes that need any logged-in user (cart, checkout,
// order history, payment pages) without a specific role. Handlers behind
// it must still scope every DB read/write to that userID — e.g.
// GetOrder(orderID, userID) rather than GetOrder(orderID) — so one user
// can never view or modify another user's cart, order, or payment by
// guessing/incrementing an ID in the URL (IDOR).
func RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := GetUserID(r); !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// RequireRole redirects an unauthenticated request to /login and 403s an
// authenticated one whose role doesn't match. Put this on every
// seller-only route (create product, delete product, seller dashboard).
//
// This is the actual access-control boundary. Anything a form's
// <select name="role"> sets only ever applies to that account's own role
// at registration time — it must never be re-read from a request to
// decide what an existing session may do. Role is always re-checked here
// from the database via the session's user ID, never from anything the
// client sends on the privileged request itself.
func RequireRole(db *store.DB, role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := GetUserID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		user, err := db.GetUserByID(userID)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if user.Role != role {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// SecurityHeaders adds standard hardening headers to every response.
// Wrap this around the whole handler chain in main.go, outside Auth, so
// headers are present even on error/redirect responses.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
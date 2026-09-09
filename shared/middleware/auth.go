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

func RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(UserIDKey) == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
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

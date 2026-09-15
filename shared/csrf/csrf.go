// Package csrf implements a small double-submit-cookie CSRF check shared
// by every service that renders a form. Call IssueCookie on any GET that
// renders one, and Verify on the POST that submits it.
package csrf

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"
)

const CookieName = "csrf_token"
const FieldName = "csrf_token"

// IssueCookie generates a new token, sets it as a cookie, and returns it
// so the caller can render it into a hidden form field (e.g.
// <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">).
func IssueCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating csrf token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   IsTLS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(24 * time.Hour),
	})
	return token, nil
}

// Verify checks that the token in the submitted form matches the cookie.
// Call r.ParseForm() before this if you haven't already.
func Verify(r *http.Request) error {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return fmt.Errorf("missing csrf cookie")
	}
	formToken := r.FormValue(FieldName)
	if formToken == "" {
		return fmt.Errorf("missing csrf token")
	}
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formToken)) != 1 {
		return fmt.Errorf("invalid csrf token")
	}
	return nil
}

// IsTLS reports whether the request arrived over HTTPS, used to decide
// whether cookies should carry the Secure flag. Trust the
// X-Forwarded-Proto header only if you terminate TLS at a reverse proxy
// you control that strips/overwrites any client-supplied value for it.
func IsTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}
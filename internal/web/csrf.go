package web

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	csrfTokenLength = 32
	csrfCookieName  = "_csrf"
	csrfHeaderName  = "X-CSRF-Token"
	csrfMaxAge      = 12 * time.Hour
)

// CSRFMiddleware provides double-submit cookie CSRF protection.
// For state-changing requests (POST/PUT/PATCH/DELETE), it validates that
// the X-CSRF-Token header matches the _csrf cookie value.
// GET/HEAD/OPTIONS requests get a CSRF cookie set if not present.
type CSRFMiddleware struct {
	logger *slog.Logger
}

// NewCSRFMiddleware creates CSRF protection middleware.
func NewCSRFMiddleware(logger *slog.Logger) *CSRFMiddleware {
	return &CSRFMiddleware{
		logger: logger,
	}
}

// Wrap returns an http.Handler enforcing CSRF on mutating methods.
func (cm *CSRFMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Non-mutating methods: ensure CSRF cookie exists, pass through.
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			cm.ensureCookie(w, r)
			// Expose token in response header for htmx to pick up.
			if cookie, err := r.Cookie(csrfCookieName); err == nil {
				w.Header().Set("X-CSRF-Token", cookie.Value)
			}
			next.ServeHTTP(w, r)
			return
		}

		// Mutating methods: validate token.
		cookie, err := r.Cookie(csrfCookieName)
		if err != nil || cookie.Value == "" {
			cm.logger.Warn("CSRF: missing cookie",
				"method", r.Method,
				"path", r.URL.Path)
			http.Error(w, "Forbidden: missing CSRF token", http.StatusForbidden)
			return
		}

		// Accept token from header (htmx sends via hx-headers) or form field.
		token := r.Header.Get(csrfHeaderName)
		if token == "" {
			token = r.FormValue("_csrf")
		}

		if !secureCompare(token, cookie.Value) {
			cm.logger.Warn("CSRF: token mismatch",
				"method", r.Method,
				"path", r.URL.Path)
			http.Error(w, "Forbidden: invalid CSRF token", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ensureCookie sets the CSRF cookie if not already present.
func (cm *CSRFMiddleware) ensureCookie(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(csrfCookieName); err == nil {
		return // already has cookie
	}

	token := cm.generateToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(csrfMaxAge.Seconds()),
		HttpOnly: false, // JS needs to read it for hx-headers
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func (cm *CSRFMiddleware) generateToken() string {
	b := make([]byte, csrfTokenLength)
	if _, err := rand.Read(b); err != nil {
		// Fallback: this should never happen but don't panic in production.
		return hex.EncodeToString(b[:16])
	}
	return hex.EncodeToString(b)
}

// secureCompare does constant-time comparison to prevent timing attacks.
func secureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	// Use subtle.ConstantTimeCompare equivalent via XOR accumulation.
	var result byte
	for i := range []byte(a) {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

// CSRFToken extracts the current CSRF token from the request (for template rendering).
func CSRFToken(r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookieName); err == nil {
		return cookie.Value
	}
	return ""
}

// CSRFMetaTag returns an HTML meta tag for the CSRF token that htmx can use.
func CSRFMetaTag(r *http.Request) string {
	token := CSRFToken(r)
	if token == "" {
		return ""
	}
	return `<meta name="csrf-token" content="` + strings.ReplaceAll(token, `"`, `&quot;`) + `">`
}

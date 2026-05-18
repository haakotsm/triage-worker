package web

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

type contextKey string

const userContextKey contextKey = "user"

// AuthUser represents an authenticated user extracted from upstream auth headers.
type AuthUser struct {
	Email string
	Name  string
	// Groups can be used for future RBAC.
	Groups []string
}

// UserFromContext extracts the authenticated user from the request context.
func UserFromContext(ctx context.Context) *AuthUser {
	u, _ := ctx.Value(userContextKey).(*AuthUser)
	return u
}

// AuthMiddleware validates that requests pass through an upstream authentication
// proxy (e.g., OAuth2-proxy, Keycloak Gatekeeper). It expects standard proxy headers:
//   - X-Auth-Request-Email: user email
//   - X-Auth-Request-User: username
//   - X-Auth-Request-Groups: comma-separated group list
//
// In development mode (devMode=true), unauthenticated requests are allowed
// with a synthetic dev user.
type AuthMiddleware struct {
	logger  *slog.Logger
	devMode bool
}

// NewAuthMiddleware creates the auth middleware.
// Set devMode=true to allow unauthenticated access for local development.
func NewAuthMiddleware(logger *slog.Logger, devMode bool) *AuthMiddleware {
	return &AuthMiddleware{logger: logger, devMode: devMode}
}

// Wrap returns an http.Handler that enforces authentication before calling next.
func (am *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets and health checks bypass auth.
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		email := r.Header.Get("X-Auth-Request-Email")
		user := r.Header.Get("X-Auth-Request-User")

		if email == "" && user == "" {
			if am.devMode {
				// In dev mode, inject a synthetic user.
				u := &AuthUser{
					Email:  "dev@localhost",
					Name:   "Developer",
					Groups: []string{"admin"},
				}
				ctx := context.WithValue(r.Context(), userContextKey, u)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			am.logger.Warn("unauthenticated request blocked",
				"path", r.URL.Path,
				"remote", r.RemoteAddr)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Extract user from proxy headers.
		u := &AuthUser{
			Email: email,
			Name:  user,
		}
		if groups := r.Header.Get("X-Auth-Request-Groups"); groups != "" {
			u.Groups = strings.Split(groups, ",")
		}

		ctx := context.WithValue(r.Context(), userContextKey, u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

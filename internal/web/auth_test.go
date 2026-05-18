package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware_AllowsStaticAssets(t *testing.T) {
	am := NewAuthMiddleware(slog.Default(), false)
	handler := am.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_AllowsHealthz(t *testing.T) {
	am := NewAuthMiddleware(slog.Default(), false)
	handler := am.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestAuthMiddleware_BlocksUnauthenticatedInProdMode(t *testing.T) {
	am := NewAuthMiddleware(slog.Default(), false)
	handler := am.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestAuthMiddleware_AllowsUnauthenticatedInDevMode(t *testing.T) {
	am := NewAuthMiddleware(slog.Default(), true)
	var capturedUser *AuthUser
	handler := am.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedUser == nil {
		t.Fatal("expected dev user in context")
	}
	if capturedUser.Email != "dev@localhost" {
		t.Errorf("expected dev@localhost, got %s", capturedUser.Email)
	}
}

func TestAuthMiddleware_ExtractsProxyHeaders(t *testing.T) {
	am := NewAuthMiddleware(slog.Default(), false)
	var capturedUser *AuthUser
	handler := am.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Request-Email", "alice@example.com")
	req.Header.Set("X-Auth-Request-User", "alice")
	req.Header.Set("X-Auth-Request-Groups", "team-a,admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedUser == nil {
		t.Fatal("expected user in context")
	}
	if capturedUser.Email != "alice@example.com" {
		t.Errorf("expected alice@example.com, got %s", capturedUser.Email)
	}
	if capturedUser.Name != "alice" {
		t.Errorf("expected alice, got %s", capturedUser.Name)
	}
	if len(capturedUser.Groups) != 2 || capturedUser.Groups[0] != "team-a" {
		t.Errorf("expected [team-a admin], got %v", capturedUser.Groups)
	}
}

func TestAuthMiddleware_AllowsEmailOnly(t *testing.T) {
	am := NewAuthMiddleware(slog.Default(), false)
	var capturedUser *AuthUser
	handler := am.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Request-Email", "bob@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if capturedUser == nil || capturedUser.Email != "bob@example.com" {
		t.Errorf("expected bob@example.com user in context")
	}
}

func TestUserFromContext_NilWhenMissing(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	u := UserFromContext(req.Context())
	if u != nil {
		t.Errorf("expected nil user, got %+v", u)
	}
}

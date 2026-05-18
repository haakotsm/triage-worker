package web

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCSRFMiddleware_SetsCookieOnGET(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	cookies := rec.Result().Cookies()
	var found bool
	for _, c := range cookies {
		if c.Name == csrfCookieName {
			found = true
			if len(c.Value) != csrfTokenLength*2 { // hex encoded
				t.Errorf("expected token length %d, got %d", csrfTokenLength*2, len(c.Value))
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("expected SameSiteStrict, got %d", c.SameSite)
			}
		}
	}
	if !found {
		t.Error("expected CSRF cookie to be set")
	}
}

func TestCSRFMiddleware_DoesNotResetExistingCookie(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "existing-token"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should not set a new cookie.
	cookies := rec.Result().Cookies()
	for _, c := range cookies {
		if c.Name == csrfCookieName {
			t.Error("should not reset existing CSRF cookie")
		}
	}
}

func TestCSRFMiddleware_BlocksPOSTWithoutCookie(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodPost, "/reports/1/resolve", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_BlocksPOSTWithMismatchedToken(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodPost, "/reports/1/resolve", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "cookie-token"})
	req.Header.Set(csrfHeaderName, "wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_AllowsPOSTWithMatchingToken(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	req := httptest.NewRequest(http.MethodPost, "/reports/1/resolve", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	req.Header.Set(csrfHeaderName, token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_AllowsPUTWithMatchingToken(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	token := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	req := httptest.NewRequest(http.MethodPut, "/reports/1", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	req.Header.Set(csrfHeaderName, token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestCSRFMiddleware_AllowsHEADWithoutToken(t *testing.T) {
	cm := NewCSRFMiddleware(slog.Default())
	handler := cm.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodHead, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestSecureCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"abc", "abc", true},
		{"abc", "def", false},
		{"abc", "ab", false},
		{"", "", true},
		{"a", "b", false},
	}
	for _, tt := range tests {
		got := secureCompare(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("secureCompare(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCSRFToken_ExtractsFromCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "my-token"})
	got := CSRFToken(req)
	if got != "my-token" {
		t.Errorf("expected my-token, got %s", got)
	}
}

func TestCSRFToken_EmptyWhenNoCookie(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got := CSRFToken(req)
	if got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

func TestCSRFMetaTag(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "test-token"})
	got := CSRFMetaTag(req)
	expected := `<meta name="csrf-token" content="test-token">`
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}

func TestCSRFMetaTag_EmptyWhenNoToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	got := CSRFMetaTag(req)
	if got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

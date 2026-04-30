package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTokenProvider_FetchesToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}

		ct := r.Header.Get("Content-Type")
		if ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type = %q, want application/x-www-form-urlencoded", ct)
		}

		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q, want client_credentials", r.Form.Get("grant_type"))
		}
		if r.Form.Get("client_id") != "test-client" {
			t.Errorf("client_id = %q, want test-client", r.Form.Get("client_id"))
		}
		if r.Form.Get("client_secret") != "test-secret" {
			t.Errorf("client_secret = %q, want test-secret", r.Form.Get("client_secret"))
		}

		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "test-token-123",
			ExpiresIn:   300,
			TokenType:   "Bearer",
		})
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "test-client", "test-secret")

	token, err := tp.Token(context.Background())
	if err != nil {
		t.Fatalf("Token() error = %v", err)
	}
	if token != "test-token-123" {
		t.Errorf("token = %q, want %q", token, "test-token-123")
	}
}

func TestTokenProvider_CachesToken(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "cached-token",
			ExpiresIn:   300,
			TokenType:   "Bearer",
		})
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "client", "secret")

	// Call twice — should only hit server once
	_, _ = tp.Token(context.Background())
	token, err := tp.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "cached-token" {
		t.Errorf("token = %q, want %q", token, "cached-token")
	}
	if callCount != 1 {
		t.Errorf("server called %d times, want 1 (should cache)", callCount)
	}
}

func TestTokenProvider_RefreshesExpiredToken(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "token-v" + string(rune('0'+callCount)),
			ExpiresIn:   1, // 1 second — will expire fast
			TokenType:   "Bearer",
		})
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "client", "secret")

	_, _ = tp.Token(context.Background())

	// Force expiry by manipulating internal state
	tp.mu.Lock()
	tp.expAt = time.Now().Add(-1 * time.Minute)
	tp.mu.Unlock()

	_, err := tp.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 2 {
		t.Errorf("server called %d times, want 2 (should refresh)", callCount)
	}
}

func TestTokenProvider_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "client", "secret")

	_, err := tp.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestTokenProvider_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "client", "secret")

	_, err := tp.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestTokenProvider_Concurrent(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "concurrent-token",
			ExpiresIn:   300,
			TokenType:   "Bearer",
		})
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "client", "secret")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := tp.Token(context.Background())
			if err != nil {
				t.Errorf("Token() error = %v", err)
			}
			if token != "concurrent-token" {
				t.Errorf("token = %q", token)
			}
		}()
	}
	wg.Wait()

	// Due to mutex serialization, server should be called very few times
	mu.Lock()
	defer mu.Unlock()
	if callCount > 3 {
		t.Errorf("server called %d times (too many for concurrent cached access)", callCount)
	}
}

func TestTokenProvider_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay to allow context cancellation
		time.Sleep(100 * time.Millisecond)
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "slow-token",
			ExpiresIn:   300,
		})
	}))
	defer server.Close()

	tp := NewTokenProvider(server.URL, "client", "secret")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := tp.Token(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

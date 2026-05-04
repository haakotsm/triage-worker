package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenProvider manages OAuth2 client-credentials tokens with caching.
type TokenProvider struct {
	tokenURL     string
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mu    sync.Mutex
	token string
	expAt time.Time
}

// NewTokenProvider creates a token provider for Keycloak client-credentials flow.
func NewTokenProvider(tokenURL, clientID, clientSecret string) *TokenProvider {
	return &TokenProvider{
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// Token returns a valid access token, refreshing if necessary.
// Thread-safe via mutex.
func (p *TokenProvider) Token(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Return cached token if still valid.
	// Buffer must exceed the longest activity timeout (120s for agent calls)
	// to prevent mid-request token expiry.
	if p.token != "" && time.Now().Add(150*time.Second).Before(p.expAt) {
		return p.token, nil
	}

	// Fetch new token
	token, expAt, err := p.fetchToken(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch token: %w", err)
	}

	p.token = token
	p.expAt = expAt
	return token, nil
}

func (p *TokenProvider) fetchToken(ctx context.Context) (string, time.Time, error) {
	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {p.clientID},
		"client_secret": {p.clientSecret},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", time.Time{}, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var tokenResp tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", time.Time{}, fmt.Errorf("decode token response: %w", err)
	}

	expAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return tokenResp.AccessToken, expAt, nil
}

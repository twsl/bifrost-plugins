package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/twsl/bifrost/codex-sub-plugin/login"
)

// TokenManager manages ChatGPT OAuth access tokens with automatic refresh.
//
// It supports three token sources, in priority order:
//  1. Direct access_token from plugin config (backward compatible)
//  2. Token file specified in config (optional, default ~/.bifrost/auth.json)
//  3. Default token file path (~/.bifrost/auth.json)
//
// When a 401 is received, the manager attempts to refresh the token using
// the stored refresh token before returning an error.
type TokenManager struct {
	mu           sync.Mutex
	config       *PluginConfig
	accessToken  string // cached access token
	refreshToken string // cached refresh token for refresh attempts
	tokenFile    string // path to the auth tokens JSON file
	issuer       string // OAuth issuer URL for token refresh
	clientID     string // OAuth client ID for token refresh
}

// NewTokenManager creates a TokenManager from the plugin config.
//
// It resolves the token source (direct or file) and initializes the
// cached access token. Returns an error if no valid token can be found.
func NewTokenManager(cfg *PluginConfig) (*TokenManager, error) {
	tm := &TokenManager{
		config: cfg,
	}

	// Resolve token file path
	tm.tokenFile = cfg.TokenFile
	if tm.tokenFile == "" {
		var err error
		tm.tokenFile, err = login.DefaultTokenFilePath()
		if err != nil {
			return nil, fmt.Errorf("resolve default token file: %w", err)
		}
	}

	// Resolve OAuth endpoints
	tm.issuer = cfg.AuthIssuer
	if tm.issuer == "" {
		tm.issuer = login.DefaultIssuer
	}
	tm.clientID = cfg.AuthClientID
	if tm.clientID == "" {
		tm.clientID = login.DefaultClientID
	}

	// Load the initial access token. Missing tokens are NOT an error here:
	// the plugin loads unauthenticated and surfaces the device-flow auth URL
	// via logs/status/description on demand (see Init and PreLLMHook).
	if err := tm.loadToken(); err != nil {
		fmt.Printf("codex-sub: no token available yet (plugin will be unauthenticated): %v\n", err)
	}

	return tm, nil
}

// GetAccessToken returns the current access token.
//
// If the cached token has expired (and a refresh token is available),
// it is silently refreshed before being returned.
func (tm *TokenManager) GetAccessToken() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Check if the cached token is expired (if it's a JWT)
	if tm.accessToken != "" && tm.refreshToken != "" {
		if isJWTExpired(tm.accessToken) {
			if err := tm.refreshLocked(); err != nil {
				// Refresh failed; return the (possibly expired) token.
				// The caller will get a 401 and handle fallthrough.
				return tm.accessToken
			}
		}
	}

	return tm.accessToken
}

// Refresh attempts to refresh the access token using the stored refresh token.
//
// This is called on 401 responses to transparently recover from expiry.
// Returns the new access token, or an error if refresh fails.
func (tm *TokenManager) Refresh() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.refreshToken == "" {
		return "", fmt.Errorf("no refresh token available")
	}

	if err := tm.refreshLocked(); err != nil {
		return "", err
	}

	return tm.accessToken, nil
}

// HasRefreshToken reports whether a refresh token is available.
func (tm *TokenManager) HasRefreshToken() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.refreshToken != ""
}

// Authenticated reports whether a usable access token is available.
// A token is considered usable if it is non-empty and, when it is a JWT,
// not yet expired (with the standard grace period).
func (tm *TokenManager) Authenticated() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.accessToken == "" {
		return false
	}
	return !isJWTExpired(tm.accessToken)
}

// PersistTokens stores an OAuth token set obtained from the device flow into
// the token file and applies it to the in-memory cache so the client begins
// using it immediately.
func (tm *TokenManager) PersistTokens(tokens *login.Tokens) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tokens == nil || tokens.AccessToken == "" {
		return fmt.Errorf("cannot persist empty token set")
	}
	tm.accessToken = tokens.AccessToken
	if tokens.RefreshToken != "" {
		tm.refreshToken = tokens.RefreshToken
	}
	return login.SaveTokens(tm.tokenFile, tokens)
}

// TokenFile returns the resolved path of the token file this manager writes to.
func (tm *TokenManager) TokenFile() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.tokenFile
}

// DeviceFlowTarget returns the values needed to run the on-demand device flow:
// the OAuth issuer and client ID configured for this plugin.
func (tm *TokenManager) DeviceFlowTarget() (issuer, clientID string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.issuer, tm.clientID
}

// loadToken loads the access token from the configured source.
//
// Priority:
//  1. Direct access_token from config
//  2. Token file
func (tm *TokenManager) loadToken() error {
	// Priority 1: direct access_token
	if tm.config.AccessToken != "" {
		tm.accessToken = tm.config.AccessToken
		tm.refreshToken = "" // no refresh token when using direct config
		return nil
	}

	// Priority 2: token file
	tokens, err := login.LoadTokens(tm.tokenFile)
	if err != nil {
		return fmt.Errorf("read token file %s: %w", tm.tokenFile, err)
	}
	if tokens == nil || tokens.AccessToken == "" {
		return fmt.Errorf("no access token found in config or token file (%s); "+
			"run 'chatgpt-sub-login' to authenticate", tm.tokenFile)
	}

	tm.accessToken = tokens.AccessToken
	tm.refreshToken = tokens.RefreshToken

	// Auto-refresh if the token is expired
	if tm.refreshToken != "" && isJWTExpired(tm.accessToken) {
		if err := tm.refreshLocked(); err != nil {
			// Non-fatal: let the caller try with the expired token;
			// a 401 response will trigger a fresh refresh attempt.
			return nil
		}
	}

	return nil
}

// refreshLocked performs the OAuth token refresh.
// Must be called with tm.mu held.
func (tm *TokenManager) refreshLocked() error {
	if tm.refreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	newTokens, err := login.RefreshTokens(tm.issuer, tm.clientID, tm.refreshToken)
	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	tm.accessToken = newTokens.AccessToken
	if newTokens.RefreshToken != "" {
		tm.refreshToken = newTokens.RefreshToken
	}

	// Persist updated tokens to file
	if err := login.SaveTokens(tm.tokenFile, &login.Tokens{
		IDToken:      newTokens.IDToken,
		AccessToken:  newTokens.AccessToken,
		RefreshToken: tm.refreshToken,
	}); err != nil {
		// Non-fatal: we still have the new token in memory
		return nil
	}

	return nil
}

// isJWTExpired checks whether a JWT-format token has an expired "exp" claim.
//
// If the token is not a valid JWT (opaque token), it returns false since
// we can't determine expiry from the token itself.
func isJWTExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false // not a JWT, can't check expiry
	}

	// Decode the payload (second segment)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Try with padding
		payload, err = base64.RawStdEncoding.DecodeString(parts[1])
		if err != nil {
			// Try standard base64 with padding
			payload, err = base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				return false // can't decode, assume not expired
			}
		}
	}

	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}

	// Allow a 5-minute grace period
	return time.Now().Unix() >= claims.Exp-300
}

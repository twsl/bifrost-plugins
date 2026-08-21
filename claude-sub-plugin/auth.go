package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/twsl/bifrost/claude-sub-plugin/login"
)

// TokenManager manages Claude Code OAuth access tokens with automatic refresh.
type TokenManager struct {
	mu           sync.Mutex
	config       *PluginConfig
	accessToken  string
	refreshToken string
	expiresAt    int64
	tokenFile    string
	clientID     string
}

// NewTokenManager creates a TokenManager, importing credentials from the
// highest-priority available source.
//
// Priority order:
//  1. Direct access_token in config
//  2. Credentials file (config override, or ~/.claude/.credentials.json)
//  3. macOS Keychain (service "Claude Code-credentials")
//  4. Token file previously written by this plugin (~/.bifrost/claude-auth.json)
func NewTokenManager(cfg *PluginConfig) (*TokenManager, error) {
	tm := &TokenManager{
		config:   cfg,
		clientID: cfg.ClientID,
	}
	if tm.clientID == "" {
		tm.clientID = DefaultClientID
	}

	tm.tokenFile = cfg.TokenFile
	if tm.tokenFile == "" {
		p, err := login.DefaultTokenFilePath()
		if err != nil {
			return nil, err
		}
		tm.tokenFile = p
	}

	// Load the initial token. Missing tokens are NOT an error: the plugin loads
	// unauthenticated and surfaces the auth URL on demand (see GetAuthStatus
	// and PreLLMHook), so logs/status/description can guide the user to
	// authenticate.
	if err := tm.loadToken(); err != nil {
		fmt.Printf("claude-sub: no token available yet (plugin will be unauthenticated): %v\n", err)
	}
	return tm, nil
}

// Authenticated reports whether a usable Claude access token is available.
func (tm *TokenManager) Authenticated() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.accessToken != "" && !tm.isExpired()
}

// PersistTokens stores an OAuth token set (from an authorization-code flow)
// into the token file and applies it in-memory immediately.
func (tm *TokenManager) PersistTokens(tok login.Tokens) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tok.AccessToken == "" {
		return fmt.Errorf("cannot persist empty Claude token set")
	}
	tm.apply(tok)
	tm.saveTokenFile()
	return nil
}

// ClientID returns the OAuth client ID used by this plugin.
func (tm *TokenManager) ClientID() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.clientID
}

// TokenFile returns the resolved path of the token file this manager writes to.
func (tm *TokenManager) TokenFile() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.tokenFile
}

// GetAccessToken returns the current access token, refreshing if expired.
func (tm *TokenManager) GetAccessToken() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.accessToken != "" && tm.refreshToken != "" && tm.isExpired() {
		if err := tm.refreshLocked(); err != nil {
			return tm.accessToken
		}
	}
	return tm.accessToken
}

// Refresh forces a refresh using the stored refresh token.
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

func (tm *TokenManager) isExpired() bool {
	if tm.expiresAt == 0 {
		return false
	}
	// 5-minute grace period
	return time.Now().Unix() >= tm.expiresAt-300
}

func (tm *TokenManager) loadToken() error {
	// Priority 1: direct config token
	if tm.config.AccessToken != "" {
		tm.accessToken = tm.config.AccessToken
		return nil
	}

	// Priority 2/3: import from Claude Code credential sources
	if tok, ok := login.Import(tm.config.CredentialsFile, tm.config.UseKeychain); ok {
		tm.apply(tok)
	}

	// Priority 4: token file written by this plugin
	if tm.accessToken == "" {
		if tok, ok := tm.loadFromTokenFile(); ok {
			tm.apply(tok)
		}
	}

	if tm.accessToken == "" {
		return fmt.Errorf("no Claude access token found; run 'claude-sub-login' " +
			"or import credentials from the Claude Code CLI")
	}

	if tm.refreshToken != "" && tm.isExpired() {
		if err := tm.refreshLocked(); err != nil {
			return nil
		}
	}
	return nil
}

func (tm *TokenManager) apply(tok login.Tokens) {
	tm.accessToken = tok.AccessToken
	tm.refreshToken = tok.RefreshToken
	tm.expiresAt = tok.ExpiresAt
}

// loadFromTokenFile reads tokens previously persisted by this plugin.
func (tm *TokenManager) loadFromTokenFile() (login.Tokens, bool) {
	data, err := os.ReadFile(tm.tokenFile)
	if err != nil {
		return login.Tokens{}, false
	}
	var tok login.Tokens
	if err := json.Unmarshal(data, &tok); err != nil {
		return login.Tokens{}, false
	}
	if tok.AccessToken != "" {
		return tok, true
	}
	return login.Tokens{}, false
}

// refreshLocked refreshes the token via Claude's OAuth token endpoint.
func (tm *TokenManager) refreshLocked() error {
	if tm.refreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	newTokens, err := login.RefreshTokens(tm.clientID, tm.refreshToken)
	if err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}

	tm.accessToken = newTokens.AccessToken
	if newTokens.RefreshToken != "" {
		tm.refreshToken = newTokens.RefreshToken
	}
	if newTokens.ExpiresAt != 0 {
		tm.expiresAt = newTokens.ExpiresAt
	}

	tm.saveTokenFile()
	return nil
}

func (tm *TokenManager) saveTokenFile() {
	if err := os.MkdirAll(filepath.Dir(tm.tokenFile), 0700); err != nil {
		return
	}
	tok := login.Tokens{
		AccessToken:  tm.accessToken,
		RefreshToken: tm.refreshToken,
		ExpiresAt:    tm.expiresAt,
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(tm.tokenFile, data, 0600)
}

// isJWTExpired checks whether a JWT token's "exp" claim is in the past.
func isJWTExpired(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.RawStdEncoding.DecodeString(parts[1])
		if err != nil {
			payload, err = base64.StdEncoding.DecodeString(parts[1])
			if err != nil {
				return false
			}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}
	return time.Now().Unix() >= claims.Exp-300
}

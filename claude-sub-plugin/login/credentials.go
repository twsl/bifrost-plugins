// Package login provides Claude Code credential import and OAuth token
// refresh helpers shared by the claude-sub plugin and the claude-sub-login CLI.
package login

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tokens holds Claude Code OAuth tokens.
type Tokens struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // unix seconds
	Scopes           []string `json:"scopes"`
	TokenUUID        string   `json:"tokenUuid,omitempty"`
	AccountID        string   `json:"accountId,omitempty"`
	AccountEmail     string   `json:"accountEmail,omitempty"`
	OrganizationID   string   `json:"organizationId,omitempty"`
	OrganizationName string   `json:"organizationName,omitempty"`
}

// Credentials mirrors the Claude Code credential store format
// (~/.claude/.credentials.json and the macOS Keychain entry). Tokens nest
// under the "claudeAiOauth" key.
type Credentials struct {
	ClaudeAiOauth Tokens `json:"claudeAiOauth"`
}

// DefaultCredentialsPath returns ~/.claude/.credentials.json.
func DefaultCredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".claude", ".credentials.json"), nil
}

// ImportFromFile imports tokens from a Claude credentials JSON file.
func ImportFromFile(path string) (Tokens, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tokens{}, false
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Tokens{}, false
	}
	if creds.ClaudeAiOauth.AccessToken != "" {
		return creds.ClaudeAiOauth, true
	}
	return Tokens{}, false
}

// ImportFromKeychain imports tokens from the macOS Keychain
// (service "Claude Code-credentials") using the `security` CLI.
func ImportFromKeychain() (Tokens, bool) {
	if _, err := os.Stat("/usr/bin/security"); err != nil {
		return Tokens{}, false
	}
	out, err := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return Tokens{}, false
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return Tokens{}, false
	}
	var creds Credentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return Tokens{}, false
	}
	if creds.ClaudeAiOauth.AccessToken != "" {
		return creds.ClaudeAiOauth, true
	}
	return Tokens{}, false
}

// Import resolves Claude Code credentials from the most likely sources.
func Import(credentialsFile string, useKeychain bool) (Tokens, bool) {
	// Explicit credentials file first.
	if credentialsFile != "" {
		if tok, ok := ImportFromFile(credentialsFile); ok {
			return tok, true
		}
	}
	// Default credentials file.
	if p, err := DefaultCredentialsPath(); err == nil {
		if tok, ok := ImportFromFile(p); ok {
			return tok, true
		}
	}
	// macOS Keychain.
	if useKeychain {
		if tok, ok := ImportFromKeychain(); ok {
			return tok, true
		}
	}
	return Tokens{}, false
}

// RefreshTokens exchanges a refresh token for a new access token against
// Claude's OAuth token endpoint.
func RefreshTokens(clientID, refreshToken string) (Tokens, error) {
	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Tokens{}, err
	}

	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, bytes.NewReader(payload))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return Tokens{}, fmt.Errorf("token refresh returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Tokens{}, err
	}
	if tr.AccessToken == "" {
		return Tokens{}, fmt.Errorf("empty access_token in refresh response")
	}

	tok := Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
	}
	if tr.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Unix() + tr.ExpiresIn
	}
	return tok, nil
}

// DefaultTokenFilePath returns ~/.bifrost/claude-auth.json.
func DefaultTokenFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".bifrost", "claude-auth.json"), nil
}

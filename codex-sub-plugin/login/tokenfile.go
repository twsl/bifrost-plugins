package login

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AuthTokensFile represents the token file format on disk.
//
// This is compatible with Codex CLI's auth.json format, so users who
// already have Codex installed can reuse their existing tokens without
// running the login flow again.
type AuthTokensFile struct {
	// AuthMode identifies the authentication mode ("chatgpt", "apiKey", etc.).
	AuthMode string `json:"auth_mode"`

	// Tokens holds the OAuth token set.
	Tokens *Tokens `json:"tokens,omitempty"`

	// LastRefresh is the timestamp of the last token refresh.
	LastRefresh time.Time `json:"last_refresh"`
}

// Tokens holds the complete OAuth token set from the device code flow.
type Tokens struct {
	// IDToken is the OpenID Connect ID token (JWT).
	IDToken string `json:"id_token"`

	// AccessToken is the Bearer token used for API authentication.
	AccessToken string `json:"access_token"`

	// RefreshToken is used to obtain a new access token when it expires.
	RefreshToken string `json:"refresh_token"`
}

// DefaultTokenFilePath returns the default path for the token file
// (~/.bifrost/codex-auth.json). Codex uses a distinct path from the
// chatgpt-sub plugin to avoid token-file collisions.
func DefaultTokenFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(home, ".bifrost", "codex-auth.json"), nil
}

// EnsureTokenDir creates the directory for the token file if it doesn't exist.
func EnsureTokenDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return nil
}

// SaveTokens writes the tokens to a JSON file atomically.
//
// The write is performed by first writing to a temporary file in the same
// directory, then renaming it to the target path. This prevents partial writes
// from corrupting the token file.
func SaveTokens(path string, tokens *Tokens) error {
	if err := EnsureTokenDir(path); err != nil {
		return err
	}

	data := AuthTokensFile{
		AuthMode:    "chatgpt",
		Tokens:      tokens,
		LastRefresh: time.Now().UTC(),
	}

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}

	// Write to a temporary file and rename for atomicity.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, jsonData, 0600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename token file: %w", err)
	}

	return nil
}

// LoadTokens reads tokens from a JSON file at the given path.
//
// Returns (nil, nil) if the file does not exist. Returns an error if
// the file exists but cannot be parsed.
func LoadTokens(path string) (*Tokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read token file %s: %w", path, err)
	}

	var file AuthTokensFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse token file %s: %w", path, err)
	}

	return file.Tokens, nil
}

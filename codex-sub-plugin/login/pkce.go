// Package login implements an OAuth device code login flow for ChatGPT.
//
// The flow mirrors OpenAI Codex CLI's approach:
//  1. Request a device code from auth.openai.com
//  2. Show the user a verification URL and one-time code
//  3. Poll for authorization while the user authenticates
//  4. Exchange the authorization code for OAuth tokens
//  5. Persist tokens to a JSON file
//
// Tokens can later be refreshed using the refresh_token grant.
package login

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PkceCodes holds the PKCE code verifier and challenge used in the OAuth flow.
type PkceCodes struct {
	CodeVerifier  string
	CodeChallenge string
}

// GeneratePKCE generates a PKCE code verifier and its S256 challenge.
//
// The verifier is derived from 64 cryptographically random bytes,
// base64url-encoded without padding (~86 characters).
// The challenge is SHA256(verifier), base64url-encoded without padding.
func GeneratePKCE() (*PkceCodes, error) {
	bytes := make([]byte, 64)
	if _, err := rand.Read(bytes); err != nil {
		return nil, fmt.Errorf("generate random bytes: %w", err)
	}

	codeVerifier := base64.RawURLEncoding.EncodeToString(bytes)

	hash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(hash[:])

	return &PkceCodes{
		CodeVerifier:  codeVerifier,
		CodeChallenge: codeChallenge,
	}, nil
}

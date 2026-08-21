package login

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OAuth PKCE endpoints and parameters for Claude's authorization-code flow.
// These mirror what the Claude Code CLI uses to authenticate a user
// (https://github.com/anthropics/claude-code). Claude does not expose an
// RFC-8628 device-code grant: the flow is browser authorization followed by
// pasting the returned ?code back, so we surface the URL and complete the
// exchange with the code the user copies from the console callback.
const (
	AuthorizeEndpoint = "https://claude.ai/oauth/authorize"
	TokenEndpoint     = "https://claude.ai/v1/oauth/token"
	RedirectURI       = "https://console.anthropic.com/oauth/code/callback"
	// OAuthScope is the scope requested from the user.
	OAuthScope = "user:profile"
	// UserAgent matches the string the Claude Code CLI sends so Anthropic
	// treats the exchange like a first-party client.
	UserAgent = "claude-code/2.0.32"
)

// tokenEndpoint is the URL used by ExchangeCode and RefreshTokens. It defaults
// to TokenEndpoint and is overridable in tests to point at a local server.
var tokenEndpoint = TokenEndpoint

// PKCEPair holds a verifier and its S256 challenge, produced by GeneratePKCE.
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE creates a 128-byte random verifier and its SHA-256 S256
// challenge per RFC 7636.
func GeneratePKCE() (PKCEPair, error) {
	buf := make([]byte, 96)
	if _, err := rand.Read(buf); err != nil {
		return PKCEPair{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)

	h := sha256.Sum256([]byte(verifier))
	return PKCEPair{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(h[:]),
	}, nil
}

// AuthorizeURL builds the interactive user-facing authorization URL for the
// Claude OAuth authorization-code + PKCE flow. Point the user here, then
// exchange the returned ?code= for tokens via ExchangeCode.
func AuthorizeURL(clientID, state string, pkce PKCEPair) (string, error) {
	u, err := url.Parse(AuthorizeEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("code", "true")
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", RedirectURI)
	q.Set("scope", OAuthScope)
	if state != "" {
		q.Set("state", state)
	}
	if pkce.Challenge != "" {
		q.Set("code_challenge", pkce.Challenge)
		q.Set("code_challenge_method", "S256")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ExchangeCode exchanges an authorization code (copied from the browser's
// redirect to the console callback URI) for tokens using the code_verifier
// from the original PKCE pair. The request body and User-Agent match the
// Claude Code CLI exactly.
func ExchangeCode(clientID, redirectURI, state, authCode, codeVerifier string) (Tokens, error) {
	if state == "" {
		return Tokens{}, fmt.Errorf("exchange code: empty state")
	}
	body := map[string]string{
		"grant_type":    "authorization_code",
		"code":          authCode,
		"state":         state,
		"client_id":     clientID,
		"redirect_uri":  redirectURI,
		"code_verifier": codeVerifier,
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
		return Tokens{}, fmt.Errorf("token exchange returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var tr struct {
		AccessToken     string `json:"access_token"`
		RefreshToken    string `json:"refresh_token"`
		ExpiresIn       int64  `json:"expires_in"`
		Scope           string `json:"scope"`
		TokenUUID       string `json:"token_uuid"`
		Account         *struct {
			UUID         string `json:"uuid"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
		Organization *struct {
			UUID string `json:"uuid"`
			Name string `json:"name"`
		} `json:"organization"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return Tokens{}, err
	}
	if tr.AccessToken == "" {
		return Tokens{}, fmt.Errorf("empty access_token in exchange response")
	}

	tok := Tokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenUUID:    tr.TokenUUID,
	}
	if tr.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Unix() + tr.ExpiresIn
	}
	if tr.Scope != "" {
		tok.Scopes = strings.Fields(tr.Scope)
	}
	if tr.Account != nil {
		tok.AccountID = tr.Account.UUID
		tok.AccountEmail = tr.Account.EmailAddress
	}
	if tr.Organization != nil {
		tok.OrganizationID = tr.Organization.UUID
		tok.OrganizationName = tr.Organization.Name
	}
	return tok, nil
}

// ExchangeCodeDefault is a convenience wrapper using the standard Claude
// console redirect URI. state should be the same value embedded in the
// authorize URL that produced authCode.
func ExchangeCodeDefault(clientID, state, authCode, codeVerifier string) (Tokens, error) {
	return ExchangeCode(clientID, RedirectURI, state, authCode, codeVerifier)
}

// ParseAuthorizationCodeInput parses the value the user pastes back after
// approving in the browser. The browser redirects to the console callback URI
// carrying "?code=...&state=...", so the pasted value is usually "code#state"
// (or the full callback URL). This matches the Claude Code CLI / yapcap
// convention and validates state for CSRF protection. Returns (code, state).
func ParseAuthorizationCodeInput(input, expectedState string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", fmt.Errorf("no authorization code pasted")
	}

	code := ""
	state := ""

	// A "?" indicates the user pasted a full callback URL or query fragment.
	if i := strings.IndexByte(input, '?'); i >= 0 {
		q, err := url.ParseQuery(input[i+1:])
		if err != nil {
			return "", "", fmt.Errorf("malformed authorization input: %q", input)
		}
		code = q.Get("code")
		state = q.Get("state")
	} else if j := strings.IndexByte(input, '#'); j >= 0 {
		// "code#state" form.
		code = input[:j]
		state = input[j+1:]
	} else {
		// Bare code with no state.
		code = input
	}

	code = strings.TrimSpace(code)
	state = strings.TrimSpace(state)
	if code == "" {
		return "", "", fmt.Errorf("no authorization code found in %q", input)
	}
	// Prefer an explicit query state when present; fall back to expectedState.
	if state == "" {
		state = expectedState
	}
	if expectedState != "" && state != expectedState {
		return "", "", fmt.Errorf("authorization code state mismatch (possible CSRF or stale flow)")
	}
	return code, state, nil
}

// SaveAuthTokens writes an OAuth token set to the plugin's token file so the
// plugin can pick it up on the next load/check without re-authenticating.
func SaveAuthTokens(path string, tok Tokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}
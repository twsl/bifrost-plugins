package login

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default constants for the OpenAI OAuth endpoint.
const (
	// DefaultIssuer is the default OAuth issuer URL for OpenAI authentication.
	DefaultIssuer = "https://auth.openai.com"

	// DefaultClientID is the OAuth client ID used by Codex CLI.
	// Extracted from codex-rs/login/src/auth/manager.rs.
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// DefaultPollTimeout is the maximum time to wait for the user to
	// complete authentication in the browser.
	DefaultPollTimeout = 15 * time.Minute
)

// DeviceCodeResponse holds the response from the initial device code request.
type DeviceCodeResponse struct {
	// VerificationURL is the URL the user visits to authenticate.
	VerificationURL string

	// UserCode is the one-time code the user enters after signing in.
	UserCode string

	// DeviceAuthID identifies this device code session for polling.
	DeviceAuthID string

	// Interval is how often to poll for authorization.
	Interval time.Duration
}

// ExchangeCodeResult holds the result of a successful token poll,
// containing the authorization code and PKCE parameters needed for
// the final token exchange.
type ExchangeCodeResult struct {
	AuthorizationCode string
	CodeVerifier      string
	CodeChallenge     string
}

// deviceCodeRequest is sent to the /device/code endpoint.
type deviceCodeRequest struct {
	ClientID string `json:"client_id"`
}

// deviceCodeResponse is received from the /device/code endpoint.
type deviceCodeResponse struct {
	UserCode     string `json:"user_code"`
	DeviceAuthID string `json:"device_auth_id"`
	Interval     int    `json:"interval"`
}

// tokenPollRequest is sent to the /deviceauth/token endpoint.
type tokenPollRequest struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

// tokenPollResponse is received from the /deviceauth/token endpoint on success.
type tokenPollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeVerifier      string `json:"code_verifier"`
	CodeChallenge     string `json:"code_challenge"`
}

// RequestDeviceCode requests a device code from the OAuth server.
//
// It POSTs to the /api/accounts/device/code endpoint and returns the
// verification URL, user code, and polling parameters.
func RequestDeviceCode(issuer, clientID string) (*DeviceCodeResponse, error) {
	baseURL := strings.TrimRight(issuer, "/")
	apiURL := baseURL + "/api/accounts/device/code"

	body := deviceCodeRequest{ClientID: clientID}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal device code request: %w", err)
	}

	resp, err := http.Post(apiURL, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device code endpoint returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var dcResp deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcResp); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}

	interval := time.Duration(dcResp.Interval) * time.Second
	if interval < 1*time.Second {
		interval = 5 * time.Second // sane default
	}

	return &DeviceCodeResponse{
		VerificationURL: baseURL + "/codex/device",
		UserCode:        dcResp.UserCode,
		DeviceAuthID:    dcResp.DeviceAuthID,
		Interval:        interval,
	}, nil
}

// PollForToken polls the token endpoint until authorization is granted
// or the timeout is reached.
//
// It POSTs to the /api/accounts/deviceauth/token endpoint at the specified
// interval, waiting for the user to complete authentication in their browser.
func PollForToken(issuer, deviceAuthID, userCode string, interval time.Duration) (*ExchangeCodeResult, error) {
	baseURL := strings.TrimRight(issuer, "/")
	pollURL := baseURL + "/api/accounts/deviceauth/token"
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()

	for {
		if time.Since(start) > DefaultPollTimeout {
			return nil, fmt.Errorf("timed out after %v waiting for authorization", DefaultPollTimeout)
		}

		body := tokenPollRequest{
			DeviceAuthID: deviceAuthID,
			UserCode:     userCode,
		}
		jsonBody, _ := json.Marshal(body)

		resp, err := client.Post(pollURL, "application/json", bytes.NewReader(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("poll request failed: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			var pollResp tokenPollResponse
			if err := json.NewDecoder(resp.Body).Decode(&pollResp); err != nil {
				resp.Body.Close()
				return nil, fmt.Errorf("decode poll response: %w", err)
			}
			resp.Body.Close()

			if pollResp.AuthorizationCode == "" {
				resp.Body.Close()
				time.Sleep(interval)
				continue
			}

			return &ExchangeCodeResult{
				AuthorizationCode: pollResp.AuthorizationCode,
				CodeVerifier:      pollResp.CodeVerifier,
				CodeChallenge:     pollResp.CodeChallenge,
			}, nil
		}

		resp.Body.Close()
		// Non-200 means still pending (authorization_pending or 404).
		time.Sleep(interval)
	}
}

// ExchangeCodeForTokens exchanges an authorization code for OAuth tokens.
//
// It POSTs to the /oauth/token endpoint with the authorization code and
// PKCE code verifier, returning the id_token, access_token, and refresh_token.
func ExchangeCodeForTokens(issuer, clientID, redirectURI, codeVerifier, authCode string) (*Tokens, error) {
	baseURL := strings.TrimRight(issuer, "/")
	tokenURL := baseURL + "/oauth/token"

	form := fmt.Sprintf(
		"grant_type=authorization_code&client_id=%s&redirect_uri=%s&code_verifier=%s&code=%s",
		urlQueryEscape(clientID),
		urlQueryEscape(redirectURI),
		urlQueryEscape(codeVerifier),
		urlQueryEscape(authCode),
	)

	resp, err := http.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		return nil, fmt.Errorf("token exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var tokenResp struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	return &Tokens{
		IDToken:      tokenResp.IDToken,
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
	}, nil
}

// RunDeviceCodeLogin runs the complete OAuth device code login flow
// and returns the obtained tokens.
//
// Steps:
//  1. Request a device code from the OAuth server
//  2. Poll for the user to complete authorization in their browser
//  3. Exchange the authorization code for OAuth tokens
func RunDeviceCodeLogin(issuer, clientID string) (*Tokens, error) {
	// Step 1: Request device code
	dc, err := RequestDeviceCode(issuer, clientID)
	if err != nil {
		return nil, fmt.Errorf("request device code: %w", err)
	}

	// Step 2: Poll for user authorization
	result, err := PollForToken(issuer, dc.DeviceAuthID, dc.UserCode, dc.Interval)
	if err != nil {
		return nil, fmt.Errorf("poll for token: %w", err)
	}

	// Step 3: Exchange authorization code for tokens
	baseURL := strings.TrimRight(issuer, "/")
	redirectURI := baseURL + "/deviceauth/callback"
	tokens, err := ExchangeCodeForTokens(issuer, clientID, redirectURI,
		result.CodeVerifier, result.AuthorizationCode)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}

	return tokens, nil
}

// urlQueryEscape is a helper for URL-encoding query parameter values.
func urlQueryEscape(s string) string {
	result := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			result += string(c)
		} else {
			result += fmt.Sprintf("%%%02X", c)
		}
	}
	return result
}

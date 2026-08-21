package login

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RefreshTokens uses a refresh token to obtain a new set of tokens
// (access_token and optionally a new refresh_token).
//
// It POSTs to the /oauth/token endpoint with grant_type=refresh_token.
// Some issuers rotate refresh tokens, others keep them stable;
// callers should always use the returned refresh_token if non-empty.
func RefreshTokens(issuer, clientID, refreshToken string) (*Tokens, error) {
	baseURL := strings.TrimRight(issuer, "/")
	tokenURL := baseURL + "/oauth/token"

	form := fmt.Sprintf(
		"grant_type=refresh_token&client_id=%s&refresh_token=%s",
		urlQueryEscape(clientID),
		urlQueryEscape(refreshToken),
	)

	resp, err := http.Post(tokenURL, "application/x-www-form-urlencoded", strings.NewReader(form))
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token refresh returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var tokenResp struct {
		IDToken      string `json:"id_token,omitempty"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("token refresh returned empty access_token")
	}

	result := &Tokens{
		IDToken:     tokenResp.IDToken,
		AccessToken: tokenResp.AccessToken,
	}

	// Use the new refresh token if the issuer rotated it.
	if tokenResp.RefreshToken != "" {
		result.RefreshToken = tokenResp.RefreshToken
	}

	return result, nil
}

package login

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthorizeURLShape(t *testing.T) {
	pkce := PKCEPair{Verifier: "verifier-value", Challenge: "challenge-value"}
	clientID := "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	state := "test-state-123"

	got, err := AuthorizeURL(clientID, state, pkce)
	if err != nil {
		t.Fatalf("AuthorizeURL: %v", err)
	}

	expectContains := func(want string) {
		if !strings.Contains(got, want) {
			t.Errorf("authorize URL missing %q: %s", want, got)
		}
	}
	expectContains(AuthorizeEndpoint + "?")
	expectContains("code=true")
	expectContains("client_id=" + clientID)
	expectContains("response_type=code")
	expectContains("redirect_uri=" + url.QueryEscape(RedirectURI))
	expectContains("scope=" + url.QueryEscape(OAuthScope))
	expectContains("state=" + url.QueryEscape(state))
	expectContains("code_challenge=challenge-value")
	expectContains("code_challenge_method=S256")

	if strings.Contains(got, "verifier-value") {
		t.Errorf("authorize URL must NOT contain the raw verifier: %s", got)
	}
}

func TestParseAuthorizationCodeInput(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		state   string
		wantErr bool
		wantCode string
	}{
		{"code-hash-state", "abc123#state-xyz", "state-xyz", false, "abc123"},
		{"bare-code", "abc123", "", false, "abc123"},
		{"full-callback-url", "https://console.anthropic.com/oauth/code/callback?code=abc123&state=state-xyz", "state-xyz", false, "abc123"},
		{"wrong-state-rejected", "abc123#nope", "state-xyz", true, ""},
		{"empty-rejected", "   ", "s", true, ""},
		{"callback-no-code-rejected", "https://console.anthropic.com/oauth/code/callback?error=denied", "s", true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, state, err := ParseAuthorizationCodeInput(c.input, c.state)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got code=%q state=%q", code, state)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if code != c.wantCode {
				t.Errorf("code = %q, want %q", code, c.wantCode)
			}
		})
	}
}

func TestExchangeCodeSendsClaudeRequestAndParsesTokens(t *testing.T) {
	var gotBody map[string]string
	var gotUA, gotCT, gotAccept string
	var receivedCode string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		gotUA = r.Header.Get("User-Agent")
		gotCT = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		receivedCode = gotBody["code"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token":"sk-ant-oat01-tok","refresh_token":"sk-ant-ort01-ref",
			"expires_in":28800,"scope":"user:profile","token_uuid":"tu-1",
			"account":{"uuid":"acct-1","email_address":"u@example.com"},
			"organization":{"uuid":"org-1","name":"My Org"}
		}`))
	}))
	defer srv.Close()

	orig := tokenEndpoint
	tokenEndpoint = srv.URL
	defer func() { tokenEndpoint = orig }()

	tok, err := ExchangeCode("client-1", RedirectURI, "state-1", "abc123", "verifier-1")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}

	if gotBody["grant_type"] != "authorization_code" {
		t.Errorf("grant_type = %q", gotBody["grant_type"])
	}
	if gotBody["code"] != "abc123" || gotBody["state"] != "state-1" {
		t.Errorf("code/state = %q/%q, want abc123/state-1", gotBody["code"], gotBody["state"])
	}
	if gotBody["code_verifier"] != "verifier-1" || gotBody["client_id"] != "client-1" {
		t.Errorf("verifier/client_id = %q/%q", gotBody["code_verifier"], gotBody["client_id"])
	}
	if gotUA != UserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, UserAgent)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if !strings.Contains(gotAccept, "application/json") {
		t.Errorf("Accept = %q", gotAccept)
	}
	if receivedCode != "abc123" {
		t.Errorf("received code = %q", receivedCode)
	}

	if tok.AccessToken != "sk-ant-oat01-tok" {
		t.Errorf("access token parse failed: %q", tok.AccessToken)
	}
	if tok.RefreshToken != "sk-ant-ort01-ref" {
		t.Errorf("refresh token parse failed: %q", tok.RefreshToken)
	}
	if tok.ExpiresAt <= 0 {
		t.Errorf("expiresAt not set: %d", tok.ExpiresAt)
	}
	if len(tok.Scopes) != 1 || tok.Scopes[0] != "user:profile" {
		t.Errorf("scopes = %v", tok.Scopes)
	}
	if tok.TokenUUID != "tu-1" {
		t.Errorf("token_uuid = %q", tok.TokenUUID)
	}
	if tok.AccountEmail != "u@example.com" || tok.AccountID != "acct-1" {
		t.Errorf("account = %q/%q", tok.AccountEmail, tok.AccountID)
	}
	if tok.OrganizationName != "My Org" || tok.OrganizationID != "org-1" {
		t.Errorf("org = %q/%q", tok.OrganizationName, tok.OrganizationID)
	}
}

func TestExchangeCodeEmptyStateRejected(t *testing.T) {
	if _, err := ExchangeCode("c", RedirectURI, "", "code", "ver"); err == nil {
		t.Error("expected error for empty state")
	}
}

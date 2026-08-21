package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/twsl/bifrost/claude-sub-plugin/login"
)

// AuthFlowState describes the current on-demand authentication state for the
// Claude plugin. Fields are JSON-friendly so a console/UI can render them.
type AuthFlowState struct {
	// Started is true once an authorize URL has been generated.
	Started bool `json:"started"`
	// AuthURL is the Claude OAuth authorize URL the user must open.
	AuthURL string `json:"auth_url,omitempty"`
	// state is the OAuth state token bound to the generated URL; it is used to
	// validate the paste-back code and is never serialized (CSRF secret).
	state string
	// Instructions is the multi-line text shown to the user in logs/status.
	Instructions string `json:"instructions,omitempty"`
	// CodeFormat describes the value the user must copy back after authorizing.
	CodeFormat string `json:"code_format,omitempty"`
}

// AuthFlow generates and exposes the Claude OAuth authorization-code + PKCE
// authorize URL so the plugin can surface it to the user via logs, status, and
// description when unauthenticated. Claude does not expose an RFC-8628
// device-code grant (unlike Codex); its native flow — the same one the Claude
// Code CLI uses — authorizes in the browser and then requires pasting back the
// "code#state" value from the console callback redirect. AuthFlow therefore
// mirrors the codex device-flow UX as closely as the protocol allows: it
// surfaces an authorization URL and code/state format, and the user completes
// authentication by running 'claude-sub-login --login' (which exchanges the
// pasted code) or by 'claude-sub-login' importing from the Claude Code CLI.
type AuthFlow struct {
	mu      sync.Mutex
	tm      *TokenManager
	state   AuthFlowState
	started bool
}

// NewAuthFlow creates an auth-flow bound to the token manager.
func NewAuthFlow(tm *TokenManager) *AuthFlow {
	return &AuthFlow{tm: tm}
}

// ensureStarted generates the authorize URL exactly once (or returns the
// cached one) so logs/status can print it immediately.
func (f *AuthFlow) ensureStarted() AuthFlowState {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.started {
		return f.state
	}
	f.started = true

	pkce, err := login.GeneratePKCE()
	if err != nil {
		f.state.Started = true
		f.state.AuthURL = ""
		f.state.Instructions = fmt.Sprintf("Could not start authorization flow: %v", err)
		return f.state
	}

	clientID := f.tm.ClientID()
	state := "claude-sub-" + randHex(8)

	authURL, err := login.AuthorizeURL(clientID, state, pkce)
	if err != nil {
		f.state.Started = true
		f.state.AuthURL = ""
		f.state.Instructions = fmt.Sprintf("Could not build authorize URL: %v", err)
		return f.state
	}

	f.state.Started = true
	f.state.state = state
	f.state.AuthURL = authURL
	f.state.CodeFormat = "<authorization-code>#" + state
	f.state.Instructions = fmt.Sprintf(
		"Authenticate with Claude: open %s in your browser and sign in. "+
			"After approving, the browser redirects to the Claude console callback "+
			"(redirect_uri=%s). Copy the \"code#state\" value from the address bar "+
			"(it starts with %q) and run 'claude-sub-login --login' to exchange it, "+
			"or run 'claude-sub-login' to import from the Claude Code CLI.",
		authURL, login.RedirectURI, state)
	return f.state
}

// Snapshot returns a copy of the current flow state safe to read concurrently.
// The CSRF state token is excluded from the returned copy.
func (f *AuthFlow) Snapshot() AuthFlowState {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.state
	state.state = ""
	return state
}

// randHex returns a lowercase hex string of n random bytes (for OAuth state).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

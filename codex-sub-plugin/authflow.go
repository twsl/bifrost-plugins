package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/twsl/bifrost/codex-sub-plugin/login"
)

// AuthFlowState describes the current on-demand authentication flow for the
// plugin. Fields are intentionally JSON-friendly so a console/UI can render
// them, and are read through the flow's mutex.
type AuthFlowState struct {
	// Started is true once a device code has been requested from the issuer.
	Started bool `json:"started"`
	// Pending is true while we are polling for the user to authorize.
	Pending bool `json:"pending"`
	// Completed is true once tokens were obtained and persisted.
	Completed bool `json:"completed"`
	// Failed is true if the flow could not be started or errored.
	Failed bool `json:"failed"`
	// Error is a human-readable message describing a failure, if any.
	Error string `json:"error,omitempty"`

	// AuthURL is the verification URL the user must open to authorize.
	AuthURL string `json:"auth_url,omitempty"`
	// VerificationURL is the same as AuthURL (kept for backward compat with
	// the CLI's naming).
	VerificationURL string `json:"verification_url,omitempty"`
	// UserCode is the one-time code the user enters on the verification page.
	UserCode string `json:"user_code,omitempty"`
	// ExpiresAt is the unix time at which the device code lapses.
	ExpiresAt int64 `json:"expires_at,omitempty"`

	// Instructions is the multi-line text shown to the user in logs/status.
	Instructions string `json:"instructions,omitempty"`
}

// AuthFlow runs the OAuth device-code login flow in the background, exposing
// the live verification URL and one-time code so the plugin can surface them
// to the user via logs, status, and description. It lazily starts the flow on
// first access when the plugin is unauthenticated, and auto-completes by
// persisting tokens to the token file when the user authorizes.
type AuthFlow struct {
	mu      sync.Mutex
	tm      *TokenManager
	state   AuthFlowState
	started bool // once() guard
}

// NewAuthFlow creates a flow bound to the given token manager.
func NewAuthFlow(tm *TokenManager) *AuthFlow {
	return &AuthFlow{
		tm: tm,
	}
}

// ensureStarted kicks off the background device flow exactly once. It returns
// the current state so callers can read the URL immediately without racing the
// first device-code round trip.
func (f *AuthFlow) ensureStarted() AuthFlowState {
	f.mu.Lock()
	if f.started {
		st := f.state
		f.mu.Unlock()
		return st
	}
	f.started = true
	f.mu.Unlock()

	go f.run()
	return f.Snapshot()
}

// Run starts the flow synchronously (used by the background goroutine). It
// requests a device code, records the URL/code, then polls until the user
// authorizes or the code lapses.
func (f *AuthFlow) run() {
	issuer, clientID := f.tm.DeviceFlowTarget()

	// Re-check auth before starting in case a token appeared while waiting.
	if f.tm.Authenticated() {
		f.mu.Lock()
		f.state.Completed = true
		f.mu.Unlock()
		return
	}

	dc, err := login.RequestDeviceCode(issuer, clientID)
	if err != nil {
		f.mu.Lock()
		f.state.Failed = true
		f.state.Error = err.Error()
		f.state.Instructions = fmt.Sprintf("Unable to start device flow: %v", err)
		f.mu.Unlock()
		return
	}

	f.mu.Lock()
	f.state.Started = true
	f.state.Pending = true
	f.state.AuthURL = dc.VerificationURL
	f.state.VerificationURL = dc.VerificationURL
	f.state.UserCode = dc.UserCode
	f.state.ExpiresAt = time.Now().Add(login.DefaultPollTimeout).Unix()
	f.state.Instructions = fmt.Sprintf(
		"Authenticate with your ChatGPT/Codex account: open %s and enter code %s (expires in 15 minutes).",
		dc.VerificationURL, dc.UserCode)
	f.mu.Unlock()

	// Poll for authorization. The poll returns the authorization code, or an
	// error on timeout/denial.
	result, err := login.PollForToken(issuer, dc.DeviceAuthID, dc.UserCode, dc.Interval)
	if err != nil {
		f.mu.Lock()
		f.state.Pending = false
		f.state.Failed = true
		f.state.Error = err.Error()
		f.state.Instructions = fmt.Sprintf(
			"Authorization did not complete at %s: %v. A new request will start the flow again.",
			dc.VerificationURL, err)
		f.mu.Unlock()
		return
	}

	baseURL := issuer
	redirectURI := baseURL + "/deviceauth/callback"
	tokens, err := login.ExchangeCodeForTokens(issuer, clientID, redirectURI,
		result.CodeVerifier, result.AuthorizationCode)
	if err != nil {
		f.mu.Lock()
		f.state.Pending = false
		f.state.Failed = true
		f.state.Error = err.Error()
		f.state.Instructions = fmt.Sprintf("Token exchange failed at %s: %v", dc.VerificationURL, err)
		f.mu.Unlock()
		return
	}

	if err := f.tm.PersistTokens(tokens); err != nil {
		f.mu.Lock()
		f.state.Pending = false
		f.state.Failed = true
		f.state.Error = err.Error()
		f.state.Instructions = fmt.Sprintf("Tokens obtained but could not be saved: %v", err)
		f.mu.Unlock()
		return
	}

	f.mu.Lock()
	f.state.Pending = false
	f.state.Completed = true
	f.state.Failed = false
	f.state.Instructions = "Authenticated successfully. Codex requests will now be proxied."
	f.mu.Unlock()
}

// Snapshot returns a copy of the current flow state safe to read concurrently.
func (f *AuthFlow) Snapshot() AuthFlowState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/twsl/bifrost/claude-sub-plugin/login"
)

// resetPluginState resets the package-level state between tests.
func resetPluginState() {
	claudeClient = nil
	pluginConfig = nil
	tokenManager = nil
	authFlow = nil
}

// newTestContext returns a BifrostContext with a background parent.
func newTestContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), time.Time{})
}

// buildClaudeChatRequest builds a minimal Anthropic chat request.
func buildClaudeChatRequest(model string, messages ...string) *schemas.BifrostRequest {
	input := make([]schemas.ChatMessage, 0, len(messages))
	for _, m := range messages {
		str := m
		input = append(input, schemas.ChatMessage{
			Role: schemas.ChatMessageRoleUser,
			Content: &schemas.ChatMessageContent{
				ContentStr: &str,
			},
		})
	}
	return &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.Anthropic,
			Model:    model,
			Input:    input,
		},
	}
}

// claudeSSEServer emulates the Anthropic Messages SSE stream.
func claudeSSEServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		events := []string{
			"event: message_start\n",
			"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" world\"}}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n",
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}

func TestClaudePreLLMHookShortCircuit(t *testing.T) {
	resetPluginState()

	srv := claudeSSEServer()
	defer srv.Close()

	err := Init(map[string]any{
		"access_token": "test-access-token",
		"api_base":     srv.URL,
		"models":       []any{"claude-sonnet-4"},
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := buildClaudeChatRequest("claude-sonnet-4", "write a function")

	newReq, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook returned error: %v", err)
	}
	if short == nil {
		t.Fatalf("expected short-circuit, got nil")
	}
	if short.Response == nil {
		t.Fatalf("expected response in short-circuit, got nil")
	}
	if newReq == nil {
		t.Fatalf("expected request to be returned")
	}

	resp := short.Response
	if resp.ChatResponse == nil {
		t.Fatalf("expected ChatResponse, got nil")
	}
	if resp.ChatResponse.Usage == nil {
		t.Fatalf("expected usage, got nil")
	}
	if resp.ChatResponse.Usage.TotalTokens != 12 {
		t.Fatalf("expected 12 total tokens, got %d", resp.ChatResponse.Usage.TotalTokens)
	}
	if resp.ChatResponse.Usage.PromptTokens != 10 {
		t.Fatalf("expected 10 prompt tokens, got %d", resp.ChatResponse.Usage.PromptTokens)
	}
	if resp.ChatResponse.Usage.CompletionTokens != 2 {
		t.Fatalf("expected 2 completion tokens, got %d", resp.ChatResponse.Usage.CompletionTokens)
	}

	if len(resp.ChatResponse.Choices) == 0 {
		t.Fatalf("expected at least one choice")
	}
	ch := resp.ChatResponse.Choices[0]
	if ch.Message == nil || ch.Message.Content == nil || ch.Message.Content.ContentStr == nil {
		t.Fatalf("expected message content, got nil")
	}
	if got := *ch.Message.Content.ContentStr; got != "Hello world" {
		t.Fatalf("expected content 'Hello world', got %q", got)
	}
	// Provider must be Anthropic in the response extra fields.
	if resp.ChatResponse.ExtraFields.Provider != schemas.Anthropic {
		t.Fatalf("expected provider anthropic, got %q", resp.ChatResponse.ExtraFields.Provider)
	}
}

func TestClaudePreLLMHookNonAnthropicPassesThrough(t *testing.T) {
	resetPluginState()

	srv := claudeSSEServer()
	defer srv.Close()

	if err := Init(map[string]any{
		"access_token": "test-access-token",
		"api_base":     srv.URL,
		"models":       []any{"claude-sonnet-4"},
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.OpenAI,
			Model:    "gpt-5",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}},
		},
	}

	newReq, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook returned error: %v", err)
	}
	if short != nil {
		t.Fatalf("expected nil short-circuit for non-Anthropic provider, got %v", short)
	}
	if newReq != req {
		t.Fatalf("expected original request to pass through unchanged")
	}
}

func TestClaudePreLLMHookUnmappedModelPassesThrough(t *testing.T) {
	resetPluginState()

	srv := claudeSSEServer()
	defer srv.Close()

	if err := Init(map[string]any{
		"access_token": "test-access-token",
		"api_base":     srv.URL,
		"models":       []any{"claude-sonnet-4"},
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := buildClaudeChatRequest("some-other-model", "hello")

	_, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook returned error: %v", err)
	}
	if short != nil {
		t.Fatalf("expected nil short-circuit for unlisted model, got %v", short)
	}
}

func TestClaudePreLLMHook401Fallthrough(t *testing.T) {
	resetPluginState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"invalid token"}}`))
	}))
	defer srv.Close()

	if err := Init(map[string]any{
		"access_token": "expired-token",
		"api_base":     srv.URL,
		"max_retries":  0.0,
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := buildClaudeChatRequest("claude-sonnet-4", "hello")

	// Without a refresh token the 401 cannot be recovered, so it falls through.
	newReq, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook returned error: %v", err)
	}
	if short != nil {
		t.Fatalf("expected nil short-circuit on 401 (fall through), got %v", short)
	}
	if newReq != req {
		t.Fatalf("expected original request to pass through on 401")
	}
}

// TestClaudeExportedSymbols builds the plugin .so and verifies the full bifrost
// plugin interface is exported via the symbol table.
func TestClaudeExportedSymbols(t *testing.T) {
	dir := t.TempDir()
	soPath := filepath.Join(dir, "claude-sub.so")

	cmd := exec.Command("go", "build", "-buildmode=plugin", "-o", soPath, ".")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to build plugin .so: %v\n%s", err, out)
	}

	sym := exec.Command("go", "tool", "nm", soPath)
	out, err = sym.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to inspect plugin symbols: %v\n%s", err, out)
	}
	table := string(out)

	required := []string{
		".Init",
		".GetName",
		".Cleanup",
		".PreRequestHook",
		".PreLLMHook",
		".PostLLMHook",
		".MarshalConfigForStorage",
		".RedactConfig",
	}
	for _, name := range required {
		if !strings.Contains(table, "github.com/twsl/bifrost/claude-sub-plugin"+name) {
			t.Errorf("required exported symbol %q not found in plugin .so", name)
		}
	}
}

// TestClaudeRedactConfig verifies secret masking behavior.
func TestClaudeRedactConfig(t *testing.T) {
	in := map[string]any{
		"access_token": "super-secret-token",
		"api_base":     "https://example.com",
		"max_retries":  float64(3),
		"max_tokens":   float64(2048),
	}
	out, err := RedactConfig(in)
	if err != nil {
		t.Fatalf("RedactConfig returned error: %v", err)
	}
	if out["access_token"] != "***" {
		t.Fatalf("expected access_token to be masked, got %v", out["access_token"])
	}
	if out["api_base"] != "https://example.com" {
		t.Fatalf("expected non-secret field preserved, got %v", out["api_base"])
	}

	// Canonical view: every supported option must be present, with defaults applied.
	for _, key := range []string{
		"access_token", "account_id", "credentials_file", "use_keychain",
		"token_file", "client_id", "beta_flags", "max_tokens",
		"models", "model_mapping", "api_base", "max_retries",
	} {
		if _, ok := out[key]; !ok {
			t.Errorf("canonical redacted view missing key %q", key)
		}
	}
	if out["max_retries"] != 3 {
		t.Fatalf("expected max_retries=3 preserved, got %v", out["max_retries"])
	}
	if out["max_tokens"] != 2048 {
		t.Fatalf("expected max_tokens=2048 preserved, got %v", out["max_tokens"])
	}
}

// TestClaudeRedactConfigDefaults verifies defaults are applied for unset fields.
func TestClaudeRedactConfigDefaults(t *testing.T) {
	out, err := RedactConfig(map[string]any{})
	if err != nil {
		t.Fatalf("RedactConfig returned error: %v", err)
	}
	if out["access_token"] != "" {
		t.Fatalf("expected unset access_token to be empty, got %v", out["access_token"])
	}
	if out["api_base"] != DefaultAPIBase {
		t.Fatalf("expected api_base default %q, got %v", DefaultAPIBase, out["api_base"])
	}
	if out["max_tokens"] != DefaultMaxTokens {
		t.Fatalf("expected max_tokens default %d, got %v", DefaultMaxTokens, out["max_tokens"])
	}
	if out["client_id"] != DefaultClientID {
		t.Fatalf("expected client_id default %q, got %v", DefaultClientID, out["client_id"])
	}
	if out["beta_flags"] != DefaultBetaFlags {
		t.Fatalf("expected beta_flags default %q, got %v", DefaultBetaFlags, out["beta_flags"])
	}
}

// TestClaudeDescribeConfig verifies the additive DescribeConfig export.
func TestClaudeDescribeConfig(t *testing.T) {
	desc := DescribeConfig()
	if desc == nil {
		t.Fatal("DescribeConfig returned nil")
	}
	for _, key := range []string{"access_token", "api_base", "use_keychain", "max_tokens", "beta_flags", "models", "model_mapping"} {
		entry, ok := desc[key].(map[string]any)
		if !ok {
			t.Fatalf("DescribeConfig entry %q is not a map: %T", key, desc[key])
		}
		if entry["kind"] == "" {
			t.Errorf("DescribeConfig entry %q missing kind", key)
		}
		if _, ok := entry["default"]; !ok {
			t.Errorf("DescribeConfig entry %q missing default", key)
		}
	}
	at, _ := desc["access_token"].(map[string]any)
	if at["kind"] != "secret" {
		t.Errorf("expected access_token kind=secret, got %v", at["kind"])
	}
	uk, _ := desc["use_keychain"].(map[string]any)
	if uk["kind"] != "bool" {
		t.Errorf("expected use_keychain kind=bool, got %v", uk["kind"])
	}
}

// TestClaudeSSEParser verifies the SSE parsing baseline.
func TestClaudeSSEParser(t *testing.T) {
	sse := "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"abc\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"def\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n"

	content, usage, err := parseClaudeSSE([]byte(sse))
	if err != nil {
		t.Fatalf("parseClaudeSSE error: %v", err)
	}
	if content != "abcdef" {
		t.Fatalf("expected 'abcdef', got %q", content)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 6 || usage.TotalTokens != 9 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

// TestClaudeModelResolution verifies model mapping + identity passthrough.
func TestClaudeModelResolution(t *testing.T) {
	c := &ClaudeClient{modelMap: DefaultModelMapping, config: &PluginConfig{}}

	if got := c.resolveModel("sonnet"); got != "claude-sonnet-4" {
		t.Fatalf("expected claude-sonnet-4, got %q", got)
	}
	if got := c.resolveModel("claude-sonnet-4"); got != "claude-sonnet-4" {
		t.Fatalf("expected claude-sonnet-4, got %q", got)
	}
	if got := c.resolveModel("custom-model"); got != "custom-model" {
		t.Fatalf("expected custom-model, got %q", got)
	}
}

// TestAuthFlowSurfacesAuthorizeURL verifies the unauthenticated plugin surfaces
// a correctly-shaped Claude authorize URL and code#state format without leaking
// the CSRF state token.
func TestAuthFlowSurfacesAuthorizeURL(t *testing.T) {
	// A TokenManager pointing at a temp token file so it loads with no token.
	tm, err := NewTokenManager(&PluginConfig{
		TokenFile: filepath.Join(t.TempDir(), "auth.json"),
		ClientID:  "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
	})
	if err != nil {
		t.Fatalf("NewTokenManager: %v", err)
	}
	if tm.Authenticated() {
		t.Fatal("expected unauthenticated token manager")
	}

	// Wire the package-level globals so GetAuthStatus can read them.
	resetPluginState()
	tokenManager = tm
	authFlow = NewAuthFlow(tm)
	defer resetPluginState()

	st := authFlow.ensureStarted()

	if !st.Started {
		t.Fatal("expected flow to be started")
	}
	if !strings.HasPrefix(st.AuthURL, login.AuthorizeEndpoint+"?") {
		t.Fatalf("unexpected authorize URL: %q", st.AuthURL)
	}
	wantStatePrefix := "claude-sub-"
	if !strings.HasPrefix(st.CodeFormat, "<authorization-code>#"+wantStatePrefix) {
		t.Fatalf("unexpected code_format: %q", st.CodeFormat)
	}
	if !strings.Contains(st.Instructions, "claude-sub-login --login") {
		t.Fatalf("instructions should mention the --login CLI: %q", st.Instructions)
	}

	// Snapshot must not expose the state token.
	snap := authFlow.Snapshot()
	if snap.state != "" {
		t.Fatalf("snapshot leaked state token: %q", snap.state)
	}

	// GetAuthStatus must include auth_url + code_format and no secret.
	status := GetAuthStatus()
	if status["auth_url"] != st.AuthURL {
		t.Fatalf("GetAuthStatus auth_url = %v", status["auth_url"])
	}
	if status["code_format"] == nil {
		t.Fatalf("GetAuthStatus missing code_format: %v", status)
	}
	for key, v := range status {
		if strings.Contains(key, "state_token") && v != "" {
			t.Fatalf("GetAuthStatus leaked a state value under %q", key)
		}
	}
	if status["authenticated"] != false {
		t.Fatalf("expected authenticated=false, got %v", status["authenticated"])
	}
}

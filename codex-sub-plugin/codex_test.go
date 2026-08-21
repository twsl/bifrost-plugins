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
)

// resetPluginState resets the package-level state between tests.
func resetPluginState() {
	codexClient = nil
	pluginConfig = nil
	tokenManager = nil
}

// newTestContext returns a BifrostContext with a background parent.
func newTestContext() *schemas.BifrostContext {
	return schemas.NewBifrostContext(context.Background(), time.Time{})
}

// buildChatRequest builds a minimal OpenAI chat request.
func buildChatRequest(model string, messages ...string) *schemas.BifrostRequest {
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
			Provider: schemas.OpenAI,
			Model:    model,
			Input:    input,
		},
	}
}

// codexSSEServer returns an httptest server that emulates the Codex Responses
// SSE stream for a successful, completed response.
func codexSSEServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/codex/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		// Emit a delta then a completed event with usage.
		events := []string{
			"event: response.output_text.delta\n",
			"data: {\"delta\":\"Hello\"}\n\n",
			"event: response.output_text.delta\n",
			"data: {\"delta\":\" world\"}\n\n",
			"event: response.completed\n",
			"data: {\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n",
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
}

func TestCodexPreLLMHookShortCircuit(t *testing.T) {
	resetPluginState()

	srv := codexSSEServer()
	defer srv.Close()

	err := Init(map[string]any{
		"access_token": "test-access-token",
		"api_base":     srv.URL,
		"models":       []any{"gpt-5-codex"},
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := buildChatRequest("gpt-5-codex", "write a function")

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

	// Verify content surfaced in choices.
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
}

func TestCodexPreLLMHookNonOpenAIPassesThrough(t *testing.T) {
	resetPluginState()

	srv := codexSSEServer()
	defer srv.Close()

	if err := Init(map[string]any{
		"access_token": "test-access-token",
		"api_base":     srv.URL,
		"models":       []any{"gpt-5-codex"},
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Provider: schemas.Anthropic,
			Model:    "claude-sonnet-4",
			Input:    []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser}},
		},
	}

	newReq, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook returned error: %v", err)
	}
	if short != nil {
		t.Fatalf("expected nil short-circuit for non-OpenAI provider, got %v", short)
	}
	if newReq != req {
		t.Fatalf("expected original request to pass through unchanged")
	}
}

func TestCodexPreLLMHookUnmappedModelPassesThrough(t *testing.T) {
	resetPluginState()

	srv := codexSSEServer()
	defer srv.Close()

	if err := Init(map[string]any{
		"access_token": "test-access-token",
		"api_base":     srv.URL,
		"models":       []any{"gpt-5-codex"}, // only this model is intercepted
	}); err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer Cleanup()

	ctx := newTestContext()
	req := buildChatRequest("some-other-model", "hello")

	_, short, err := PreLLMHook(ctx, req)
	if err != nil {
		t.Fatalf("PreLLMHook returned error: %v", err)
	}
	if short != nil {
		t.Fatalf("expected nil short-circuit for unlisted model, got %v", short)
	}
}

func TestCodexPreLLMHook401Fallthrough(t *testing.T) {
	resetPluginState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid token"}}`))
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
	req := buildChatRequest("gpt-5-codex", "hello")

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

// TestCodexExportedSymbols builds the plugin .so and verifies that the full
// bifrost plugin interface is exported. Because Go 'plugin' packages must be
// built in a separate process from the test binary (the test binary includes
// _test.go files and thus has a different build identity), we inspect the
// exported symbol table with 'go tool nm' rather than plugin.Open.
func TestCodexExportedSymbols(t *testing.T) {
	dir := t.TempDir()
	soPath := filepath.Join(dir, "codex-sub.so")

	cmd := exec.Command("go", "build", "-buildmode=plugin", "-o", soPath, ".")
	cmd.Dir = "." // the plugin package root (this test lives in the same dir)
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

	// Every symbol the bifrost plugin loader expects must be exported.
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
		if !strings.Contains(table, "github.com/twsl/bifrost/codex-sub-plugin"+name) {
			t.Errorf("required exported symbol %q not found in plugin .so", name)
		}
	}
}

// TestCodexRedactConfig verifies secret masking behavior.
func TestCodexRedactConfig(t *testing.T) {
	in := map[string]any{
		"access_token": "super-secret-token",
		"api_base":     "https://example.com",
		"max_retries":  float64(3),
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
		"access_token", "account_id", "token_file", "auth_issuer",
		"auth_client_id", "api_base", "max_retries", "models", "model_mapping",
	} {
		if _, ok := out[key]; !ok {
			t.Errorf("canonical redacted view missing key %q", key)
		}
	}
	if out["max_retries"] != 3 {
		t.Fatalf("expected max_retries=3 preserved, got %v", out["max_retries"])
	}
}

// TestCodexRedactConfigDefaults verifies defaults are applied for unset fields.
func TestCodexRedactConfigDefaults(t *testing.T) {
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
}

// TestCodexDescribeConfig verifies the additive DescribeConfig export.
func TestCodexDescribeConfig(t *testing.T) {
	desc := DescribeConfig()
	if desc == nil {
		t.Fatal("DescribeConfig returned nil")
	}
	for _, key := range []string{"access_token", "api_base", "models", "model_mapping", "max_retries"} {
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
}

// TestCodexSSEParser verifies the SSE parsing baseline with a crafted stream.
func TestCodexSSEParser(t *testing.T) {
	sse := "" +
		"event: response.output_text.delta\n" +
		"data: {\"delta\":\"abc\"}\n\n" +
		"event: response.output_text.delta\n" +
		"data: {\"delta\":\"def\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":6,\"total_tokens\":9}}}\n\n"

	content, usage, err := parseResponsesSSE([]byte(sse))
	if err != nil {
		t.Fatalf("parseResponsesSSE error: %v", err)
	}
	if content != "abcdef" {
		t.Fatalf("expected 'abcdef', got %q", content)
	}
	if usage.PromptTokens != 3 || usage.CompletionTokens != 6 || usage.TotalTokens != 9 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

// TestCodexModelResolution verifies model mapping + identity passthrough.
func TestCodexModelResolution(t *testing.T) {
	c := &CodexClient{modelMap: DefaultModelMapping, config: &PluginConfig{}}

	// Mapped model.
	if got := c.resolveModel("gpt-5"); got != "gpt-5-codex" {
		t.Fatalf("expected gpt-5-codex, got %q", got)
	}
	// Identity passthrough for unknown.
	if got := c.resolveModel("gpt-5-codex"); got != "gpt-5-codex" {
		t.Fatalf("expected gpt-5-codex, got %q", got)
	}
	// Unknown model passes through unchanged.
	if got := c.resolveModel("custom-model"); got != "custom-model" {
		t.Fatalf("expected custom-model, got %q", got)
	}
}

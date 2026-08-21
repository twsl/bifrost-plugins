package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// CodexClient handles communication with ChatGPT's Codex backend API.
//
// It targets the `chatgpt.com/backend-api/codex/responses` endpoint, which is
// the OpenAI Responses-API-shaped endpoint that the official Codex CLI uses
// when authenticated with a ChatGPT Plus/Pro subscription (OAuth access token).
type CodexClient struct {
	httpClient   *http.Client
	config       *PluginConfig
	modelMap     map[string]string // merged default + user overrides
	tokenManager *TokenManager
	mu           sync.Mutex
	accessToken  string // current access token (may be refreshed)
	accountID    string // chatgpt_account_id claim from the ID token
}

// NewCodexClient creates a new Codex API client.
func NewCodexClient(config *PluginConfig, tm *TokenManager) *CodexClient {
	// Merge default model mapping with user overrides
	modelMap := make(map[string]string, len(DefaultModelMapping))
	for k, v := range DefaultModelMapping {
		modelMap[k] = v
	}
	for k, v := range config.ModelMapping {
		modelMap[k] = v
	}

	return &CodexClient{
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		config:       config,
		modelMap:     modelMap,
		tokenManager: tm,
		accessToken:  config.AccessToken,
		accountID:    config.AccountID,
	}
}

// apiBase returns the configured API base URL or the default.
func (c *CodexClient) apiBase() string {
	if c.config.APIBase != "" {
		return c.config.APIBase
	}
	return DefaultAPIBase
}

// responsesURL returns the full Responses endpoint URL.
func (c *CodexClient) responsesURL() string {
	return strings.TrimRight(c.apiBase(), "/") + "/codex/responses"
}

// resolveModel maps an OpenAI model name to a Codex backend slug, falling back
// to identity passthrough when no mapping exists.
func (c *CodexClient) resolveModel(model string) string {
	if slug, ok := c.modelMap[model]; ok {
		return slug
	}
	return model
}

// TranslateAndCall translates an OpenAI-format BifrostRequest into a Codex
// backend Responses-API call, then translates the response back into a
// BifrostResponse.
func (c *CodexClient) TranslateAndCall(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostResponse, error) {
	provider, model, _ := req.GetRequestFields()

	// Resolve the Codex model slug. Unknown models pass through unchanged,
	// so an explicit codex slug (e.g. "gpt-5-codex") still reaches the backend.
	codexModel := c.resolveModel(model)

	// Build the Codex Responses request body from the inbound request.
	body, err := c.buildResponsesBody(req, codexModel)
	if err != nil {
		return nil, fmt.Errorf("build responses body: %w", err)
	}

	ctx.Log(schemas.LogLevelDebug, fmt.Sprintf("codex-sub: calling Codex API model=%s", codexModel))

	// Make the HTTP call (SSE streaming response).
	responseBody, err := c.doHTTPCall(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}

	// Parse the SSE response.
	content, usage, err := parseResponsesSSE(responseBody)
	if err != nil {
		return nil, fmt.Errorf("parse SSE response: %w", err)
	}

	return c.buildResponse(provider, model, content, usage), nil
}

// buildResponsesBody builds the JSON body for the Codex Responses endpoint.
//
// It mirrors the OpenAI Responses API input shape: a list of messages with
// role/content, plus an optional explicit "instructions" system prompt.
func (c *CodexClient) buildResponsesBody(req *schemas.BifrostRequest, model string) ([]byte, error) {
	inputItems, err := responsesInputItems(req)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"model":  model,
		"input":  inputItems,
		"stream": true,
	}

	// Forward parameters (tools, instructions, etc.) when present on a
	// Responses request.
	if req.ResponsesRequest != nil && req.ResponsesRequest.Params != nil {
		if paramsBytes, err := json.Marshal(req.ResponsesRequest.Params); err == nil {
			var paramsMap map[string]any
			if json.Unmarshal(paramsBytes, &paramsMap) == nil {
				for k, v := range paramsMap {
					payload[k] = v
				}
			}
		}
	}

	return json.Marshal(payload)
}

// responsesInputItems extracts Responses-API input messages from the request.
//
// KNOWN LIMITATION: OpenAI's codex `additional_tools` input item (used by
// code-mode models such as gpt-5.6-sol) is not reconstructed here. The bifrost
// core schema this plugin links against (core v1.6.3) has no
// `ResponsesMessage.AdditionalTools` field — that is a bifrost `dev`-branch
// addition (see core/providers/openai hoistAdditionalTools). Requests carrying
// `additional_tools` items pass their tools through top-level
// `Parameters.Tools` (forwarded verbatim in buildResponsesBody), but the
// item-level `additional_tools` array is dropped. Models that require
// `additional_tools` on first request may reject the call; consumers should
// use a model whose tooling fits top-level `tools`.
func responsesInputItems(req *schemas.BifrostRequest) ([]map[string]any, error) {
	items := make([]map[string]any, 0, 8)

	// Preferred path: a Responses request with a structured input.
	if req.ResponsesRequest != nil && req.ResponsesRequest.Input != nil {
		for _, msg := range req.ResponsesRequest.Input {
			item := map[string]any{}
			if msg.Role != nil {
				item["role"] = string(*msg.Role)
			}
			if msg.Content != nil {
				if msg.Content.ContentStr != nil {
					item["content"] = *msg.Content.ContentStr
				} else if msg.Content.ContentBlocks != nil {
					item["content"] = msg.Content.ContentBlocks
				}
			}
			if msg.Name != nil {
				item["name"] = *msg.Name
			}
			items = append(items, item)
		}
		return items, nil
	}

	// Fallback: convert chat-completion messages to Responses input items.
	if req.ChatRequest != nil && req.ChatRequest.Input != nil {
		for _, msg := range req.ChatRequest.Input {
			role := string(msg.Role)
			if role == "" {
				role = "user"
			}
			item := map[string]any{"role": role}
			if msg.Content != nil && msg.Content.ContentStr != nil {
				item["content"] = *msg.Content.ContentStr
			}
			items = append(items, item)
		}
		return items, nil
	}

	return nil, fmt.Errorf("no input found in request")
}

// doHTTPCall performs the HTTP POST to the Codex backend API.
func (c *CodexClient) doHTTPCall(ctx *schemas.BifrostContext, body []byte) ([]byte, error) {
	url := c.responsesURL()
	ctx.Log(schemas.LogLevelDebug, fmt.Sprintf("codex-sub: POST %s", url))

	var lastErr error
	attempts := 1 + c.config.MaxRetries
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			ctx.Log(schemas.LogLevelWarn, fmt.Sprintf("codex-sub: retry %d/%d after %v",
				attempt+1, attempts, backoff))
			time.Sleep(backoff)
		}

		token := c.getCurrentToken()

		httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = fmt.Errorf("create request: %w", err)
			continue
		}

		c.applyHeaders(httpReq, token)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			continue
		}

		// Handle 401 — attempt token refresh and retry once.
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			ctx.Log(schemas.LogLevelWarn, "codex-sub: received 401, attempting token refresh")

			newToken, refreshErr := c.tokenManager.Refresh()
			if refreshErr != nil {
				return nil, fmt.Errorf("Codex API returned 401 — access token expired and refresh failed: %w", refreshErr)
			}

			c.mu.Lock()
			c.accessToken = newToken
			c.mu.Unlock()

			retryReq, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			c.applyHeaders(retryReq, newToken)

			resp, err = c.httpClient.Do(retryReq)
			if err != nil {
				return nil, fmt.Errorf("http retry after refresh: %w", err)
			}
		}

		// Handle non-200 status codes.
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodyStr := truncate(strings.TrimSpace(string(bodyBytes)), 500)

			switch resp.StatusCode {
			case http.StatusTooManyRequests:
				lastErr = fmt.Errorf("Codex API returned 429 (rate limited): %s", bodyStr)
				continue // retry
			default:
				return nil, fmt.Errorf("Codex API returned %d: %s", resp.StatusCode, bodyStr)
			}
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response body: %w", err)
			continue
		}

		return respBody, nil
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", attempts, lastErr)
}

// applyHeaders sets the Codex-identity headers required by the backend.
func (c *CodexClient) applyHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Originator", "codex_cli_rs")
	if c.accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", c.accountID)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
}

// getCurrentToken returns the current access token, refreshing from the
// TokenManager if needed (thread-safe).
func (c *CodexClient) getCurrentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.tokenManager != nil {
		token := c.tokenManager.GetAccessToken()
		if token != "" {
			c.accessToken = token
		}
	}

	return c.accessToken
}

// parseResponsesSSE parses the SSE stream from the Codex Responses endpoint.
//
// The Responses API emits events shaped as:
//
//	event: response.output_text.delta
//	data: {"delta": "..."}
//
//	event: response.completed
//	data: {"response": {"usage": {...}}}
func parseResponsesSSE(data []byte) (content string, usage *schemas.BifrostLLMUsage, err error) {
	usage = &schemas.BifrostLLMUsage{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var currentEvent string
	var lastContent strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		// Whitespace (keep-alive) or non-JSON payloads are ignored.
		var raw map[string]any
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			continue
		}

		switch currentEvent {
		case "response.output_text.delta":
			if d, ok := raw["delta"].(string); ok {
				lastContent.WriteString(d)
			}
		case "response.completed":
			if resp, ok := raw["response"].(map[string]any); ok {
				usage = extractUsage(resp, usage)
			}
		case "response.failed":
			if e, ok := raw["response"].(map[string]any); ok {
				if errMsg, ok := e["error"].(map[string]any); ok {
					if msg, ok := errMsg["message"].(string); ok {
						return "", nil, fmt.Errorf("Codex API error: %s", msg)
					}
				}
			}
			return "", nil, fmt.Errorf("Codex API reported response.failed")
		case "error":
			if e, ok := raw["error"].(map[string]any); ok {
				if msg, ok := e["message"].(string); ok {
					return "", nil, fmt.Errorf("Codex API error: %s", msg)
				}
			}
			return "", nil, fmt.Errorf("Codex API returned an error event")
		}
	}

	if err := scanner.Err(); err != nil {
		return "", nil, fmt.Errorf("SSE scanner error: %w", err)
	}

	content = lastContent.String()

	// Fall back to a coarse estimate when usage was never reported.
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		usage.PromptTokens = len(content) / 4
		usage.CompletionTokens = len(content) / 4
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	return content, usage, nil
}

// extractUsage reads token counts from a completed Responses object.
func extractUsage(resp map[string]any, usage *schemas.BifrostLLMUsage) *schemas.BifrostLLMUsage {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return usage
	}
	if v, ok := u["input_tokens"].(float64); ok {
		usage.PromptTokens = int(v)
	}
	if v, ok := u["output_tokens"].(float64); ok {
		usage.CompletionTokens = int(v)
	}
	if v, ok := u["total_tokens"].(float64); ok {
		usage.TotalTokens = int(v)
	}
	return usage
}

// buildResponse constructs a BifrostResponse from the Codex response data.
func (c *CodexClient) buildResponse(provider schemas.ModelProvider, model, content string, usage *schemas.BifrostLLMUsage) *schemas.BifrostResponse {
	finishReason := "stop"
	resp := &schemas.BifrostResponse{
		ChatResponse: &schemas.BifrostChatResponse{
			Model: model,
			Usage: usage,
			Choices: []schemas.BifrostResponseChoice{
				{
					Index: 0,
					ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{
						Message: &schemas.ChatMessage{
							Role: schemas.ChatMessageRoleAssistant,
							Content: &schemas.ChatMessageContent{
								ContentStr: &content,
							},
						},
					},
					FinishReason: &finishReason,
				},
			},
			ExtraFields: schemas.BifrostResponseExtraFields{
				RequestType:            schemas.ChatCompletionRequest,
				Provider:               provider,
				OriginalModelRequested: model,
			},
		},
	}

	return resp
}

// truncate limits a string to maxLen runes, appending "..." when truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

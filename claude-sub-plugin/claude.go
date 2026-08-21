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

// ClaudeClient communicates with the Anthropic Messages API using Claude Code
// OAuth credentials (subscription auth). It targets
// `api.anthropic.com/v1/messages?beta=true` with the oauth beta flag enabled.
type ClaudeClient struct {
	httpClient   *http.Client
	config       *PluginConfig
	modelMap     map[string]string
	tokenManager *TokenManager
	mu           sync.Mutex
}

// NewClaudeClient creates a new Claude API client.
func NewClaudeClient(config *PluginConfig, tm *TokenManager) *ClaudeClient {
	modelMap := make(map[string]string, len(DefaultModelMapping))
	for k, v := range DefaultModelMapping {
		modelMap[k] = v
	}
	for k, v := range config.ModelMapping {
		modelMap[k] = v
	}
	return &ClaudeClient{
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		config:       config,
		modelMap:     modelMap,
		tokenManager: tm,
	}
}

func (c *ClaudeClient) apiBase() string {
	if c.config.APIBase != "" {
		return c.config.APIBase
	}
	return DefaultAPIBase
}

// resolveModel maps an Anthropic model name to a backend slug, falling back to
// identity passthrough when no mapping exists.
func (c *ClaudeClient) resolveModel(model string) string {
	if slug, ok := c.modelMap[model]; ok {
		return slug
	}
	return model
}

// betaFlags returns the anthropic-beta header value, preferring an explicit
// config override over the default flag set.
func (c *ClaudeClient) betaFlags() string {
	if c.config != nil && c.config.BetaFlags != "" {
		return c.config.BetaFlags
	}
	return DefaultBetaFlags
}

// maxTokens returns the max_tokens value for outgoing Messages requests.
func (c *ClaudeClient) maxTokens() int {
	if c.config != nil && c.config.MaxTokens > 0 {
		return c.config.MaxTokens
	}
	return DefaultMaxTokens
}

// TranslateAndCall translates a BifrostRequest into an Anthropic Messages API
// call and translates the response back into a BifrostResponse.
func (c *ClaudeClient) TranslateAndCall(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostResponse, error) {
	_, model, _ := req.GetRequestFields()

	claudeModel := c.resolveModel(model)

	body, systemPrompt, err := c.buildMessagesBody(req, claudeModel)
	if err != nil {
		return nil, fmt.Errorf("build messages body: %w", err)
	}

	responseBody, err := c.doHTTPCall(ctx, "/v1/messages", body)
	if err != nil {
		return nil, fmt.Errorf("http call: %w", err)
	}

	// Parse the (streamed or non-streamed) response.
	content, usage, err := parseClaudeResponse(responseBody)
	if err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return c.buildResponse(claudeModel, content, usage, systemPrompt), nil
}

// claudeMessagesBody mirrors the Anthropic Messages API request shape.
type claudeMessagesBody struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Stream        bool            `json:"stream"`
	System        claudeSystem    `json:"system"`
	Messages      []claudeMessage `json:"messages"`
	Beta          []string        `json:"-"`
}

type claudeSystem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeMessage struct {
	Role    string        `json:"role"`
	Content []claudeBlock `json:"content"`
}

type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// buildMessagesBody converts inbound request messages into the Anthropic shape.
// It returns the request body bytes and the extracted system prompt.
func (c *ClaudeClient) buildMessagesBody(req *schemas.BifrostRequest, model string) ([]byte, string, error) {
	systemPrompt := "You are Claude Code, Anthropic's official CLI for Claude."
	msgs := make([]claudeMessage, 0, 8)

	// Preferred: Responses-style input.
	if req.ResponsesRequest != nil && req.ResponsesRequest.Input != nil {
		for _, m := range req.ResponsesRequest.Input {
			role := "user"
			if m.Role != nil {
				switch *m.Role {
				case schemas.ResponsesInputMessageRoleAssistant:
					role = "assistant"
				case schemas.ResponsesInputMessageRoleSystem,
					schemas.ResponsesInputMessageRoleDeveloper:
					if m.Content != nil && m.Content.ContentStr != nil {
						systemPrompt = *m.Content.ContentStr
					}
					continue
				}
			}
			content := ""
			if m.Content != nil {
				if m.Content.ContentStr != nil {
					content = *m.Content.ContentStr
				} else if m.Content.ContentBlocks != nil {
					for _, b := range m.Content.ContentBlocks {
						if b.Text != nil {
							content += *b.Text
						}
					}
				}
			}
			msgs = append(msgs, claudeMessage{
				Role:    role,
				Content: []claudeBlock{{Type: "text", Text: content}},
			})
		}
		body := claudeMessagesBody{
			Model:     model,
			MaxTokens: c.maxTokens(),
			Stream:    true,
			System:    claudeSystem{Type: "text", Text: systemPrompt},
			Messages:  msgs,
		}
		return marshalMessagesBody(body)
	}

	// Fallback: chat-completion-style input.
	if req.ChatRequest != nil && req.ChatRequest.Input != nil {
		for _, m := range req.ChatRequest.Input {
			role := string(m.Role)
			if role == "" {
				role = "user"
			}
			if role == string(schemas.ChatMessageRoleSystem) {
				if m.Content != nil && m.Content.ContentStr != nil {
					systemPrompt = *m.Content.ContentStr
				}
				continue
			}
			content := ""
			if m.Content != nil && m.Content.ContentStr != nil {
				content = *m.Content.ContentStr
			}
			msgs = append(msgs, claudeMessage{
				Role:    role,
				Content: []claudeBlock{{Type: "text", Text: content}},
			})
		}
		body := claudeMessagesBody{
			Model:     model,
			MaxTokens: c.maxTokens(),
			Stream:    true,
			System:    claudeSystem{Type: "text", Text: systemPrompt},
			Messages:  msgs,
		}
		return marshalMessagesBody(body)
	}

	return nil, systemPrompt, fmt.Errorf("no input found in request")
}

func marshalMessagesBody(body claudeMessagesBody) ([]byte, string, error) {
	data, err := json.Marshal(body)
	return data, body.System.Text, err
}

// doHTTPCall performs the HTTP POST to the Anthropic API.
func (c *ClaudeClient) doHTTPCall(ctx *schemas.BifrostContext, path string, body []byte) ([]byte, error) {
	base := strings.SplitN(c.apiBase(), "?", 2)[0]
	url := strings.TrimRight(base, "/") + path
	if c.apiBase() != base {
		// Re-append the beta query string.
		if q := strings.SplitN(c.apiBase(), "?", 2); len(q) == 2 {
			url += "?" + q[1]
		}
	}

	ctx.Log(schemas.LogLevelDebug, fmt.Sprintf("claude-sub: POST %s", url))

	var lastErr error
	attempts := 1 + c.config.MaxRetries
	if attempts < 1 {
		attempts = 1
	}

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			time.Sleep(backoff)
		}

		token := c.tokenManager.GetAccessToken()

		httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		applyClaudeHeaders(httpReq, token, c.betaFlags())
		if c.config.AccountID != "" {
			httpReq.Header.Set("ORG-Id", c.config.AccountID)
		}

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			ctx.Log(schemas.LogLevelWarn, "claude-sub: 401, refreshing token")

			newToken, refreshErr := c.tokenManager.Refresh()
			if refreshErr != nil {
				return nil, fmt.Errorf("Claude API 401 and refresh failed: %w", refreshErr)
			}

			retryReq, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
			applyClaudeHeaders(retryReq, newToken, c.betaFlags())
			if c.config.AccountID != "" {
				retryReq.Header.Set("ORG-Id", c.config.AccountID)
			}
			resp, err = c.httpClient.Do(retryReq)
			if err != nil {
				return nil, err
			}
		}

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			bodyStr := truncateClaude(strings.TrimSpace(string(bodyBytes)), 500)
			if resp.StatusCode == http.StatusTooManyRequests {
				lastErr = fmt.Errorf("Claude API 429: %s", bodyStr)
				continue
			}
			return nil, fmt.Errorf("Claude API %d: %s", resp.StatusCode, bodyStr)
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return respBody, nil
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", attempts, lastErr)
}

// applyClaudeHeaders sets the headers the Claude Code CLI sends.
func applyClaudeHeaders(req *http.Request, token, betaFlags string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", DefaultAnthropicVersion)
	req.Header.Set("anthropic-beta", betaFlags)
	req.Header.Set("x-app", "cli")
	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	req.Header.Set("User-Agent", "claude-cli/2.1.234 (external, cli)")
}

// parseClaudeResponse parses either a streaming SSE or non-streaming JSON
// response into text content and usage.
func parseClaudeResponse(data []byte) (string, *schemas.BifrostLLMUsage, error) {
	usage := &schemas.BifrostLLMUsage{}

	// Try a non-streaming JSON body first.
	var full struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &full); err == nil && (len(full.Content) > 0 || full.Usage.InputTokens > 0) {
		var sb strings.Builder
		for _, b := range full.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		usage.PromptTokens = full.Usage.InputTokens
		usage.CompletionTokens = full.Usage.OutputTokens
		usage.TotalTokens = full.Usage.InputTokens + full.Usage.OutputTokens
		return sb.String(), usage, nil
	}

	// Otherwise parse SSE stream.
	content, u, err := parseClaudeSSE(data)
	if err != nil {
		return "", nil, err
	}
	return content, u, nil
}

// parseClaudeSSE parses Anthropic's streaming events:
//
//	event: content_block_delta
//	data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
//
//	event: message_delta
//	data: {"type":"message_delta","usage":{"output_tokens":N}}
//
//	event: message_start
//	data: {"type":"message_start","message":{"usage":{"input_tokens":N}}}
func parseClaudeSSE(data []byte) (string, *schemas.BifrostLLMUsage, error) {
	usage := &schemas.BifrostLLMUsage{}
	var sb strings.Builder

	scanner := bufio.NewScanner(bytes.NewReader(data))
	var currentEvent string
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
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var raw map[string]any
		if err := json.Unmarshal([]byte(payload), &raw); err != nil {
			continue
		}

		switch currentEvent {
		case "content_block_delta":
			if delta, ok := raw["delta"].(map[string]any); ok {
				if t, ok := delta["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		case "message_delta":
			if u, ok := raw["usage"].(map[string]any); ok {
				if v, ok := u["output_tokens"].(float64); ok {
					usage.CompletionTokens = int(v)
				}
			}
		case "message_start":
			if msg, ok := raw["message"].(map[string]any); ok {
				if u, ok := msg["usage"].(map[string]any); ok {
					if v, ok := u["input_tokens"].(float64); ok {
						usage.PromptTokens = int(v)
					}
				}
			}
		case "error":
			if e, ok := raw["error"].(map[string]any); ok {
				if m, ok := e["message"].(string); ok {
					return "", nil, fmt.Errorf("Claude API error: %s", m)
				}
			}
		}
	}

	content := sb.String()
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if usage.TotalTokens == 0 {
		usage.PromptTokens = len(content) / 4
		usage.CompletionTokens = len(content) / 4
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return content, usage, nil
}

// buildResponse constructs a BifrostResponse carrying the Claude output.
func (c *ClaudeClient) buildResponse(model, content string, usage *schemas.BifrostLLMUsage, _ string) *schemas.BifrostResponse {
	finishReason := "stop"
	return &schemas.BifrostResponse{
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
				Provider:               schemas.Anthropic,
				OriginalModelRequested: model,
			},
		},
	}
}

// truncateClaude limits a string to maxLen.
func truncateClaude(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

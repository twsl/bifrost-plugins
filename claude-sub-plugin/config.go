package main

import "fmt"

// PluginConfig is the configuration for the Claude Subscription plugin.
// It is deserialized from the "config" field in Bifrost's plugin configuration.
type PluginConfig struct {
	// AccessToken is the Claude Code OAuth access token.
	// If set, it takes precedence over credential-import sources.
	AccessToken string `json:"access_token"`

	// AccountID is the Anthropic organization/account ID (used for the
	// ORG-Id header on some endpoints). Optional.
	AccountID string `json:"account_id,omitempty"`

	// CredentialsFile is the path to a JSON file containing Claude OAuth
	// credentials (imported from the Claude Code CLI, ~/.claude/.credentials.json
	// or macOS Keychain). If AccessToken is empty, tokens are loaded from here.
	CredentialsFile string `json:"credentials_file,omitempty"`

	// UseKeychain enables importing the credential from macOS Keychain
	// (service "Claude Code-credentials"). Enabled by default on macOS.
	UseKeychain bool `json:"use_keychain,omitempty"`

	// TokenFile is the path where refreshed tokens are persisted
	// (default ~/.bifrost/claude-auth.json).
	TokenFile string `json:"token_file,omitempty"`

	// ClientID is the Anthropic OAuth client ID used for refresh requests.
	// Default: "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	ClientID string `json:"client_id,omitempty"`

	// BetaFlags overrides the comma-separated anthropic-beta header value.
	// When empty, DefaultBetaFlags is used. This mirrors bifrost's dynamic
	// per-feature beta gating by letting operators pin/rotate the flag set
	// without rebuilding the plugin.
	BetaFlags string `json:"beta_flags,omitempty"`

	// MaxTokens overrides the max_tokens value sent to the Anthropic Messages
	// API. When 0, DefaultMaxTokens (4096, matching bifrost's
	// AnthropicDefaultMaxTokens convention) is used.
	MaxTokens int `json:"max_tokens,omitempty"`

	// Models is the list of Anthropic model names to intercept.
	Models []string `json:"models"`

	// ModelMapping optionally overrides the default Anthropic→Claude slug mapping.
	ModelMapping map[string]string `json:"model_mapping,omitempty"`

	// APIBase optionally overrides the Anthropic API base URL.
	// Default: "https://api.anthropic.com/v1/messages?beta=true"
	APIBase string `json:"api_base,omitempty"`

	// MaxRetries optionally sets the number of retries for failed HTTP calls.
	MaxRetries int `json:"max_retries,omitempty"`
}

// DefaultAPIBase is the default Anthropic Messages API endpoint with beta flags.
const DefaultAPIBase = "https://api.anthropic.com/v1/messages?beta=true"

// DefaultAnthropicVersion is the API version header sent to Anthropic.
const DefaultAnthropicVersion = "2023-06-01"

// DefaultBetaFlags are the beta feature flags the Claude Code CLI sends.
const DefaultBetaFlags = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,effort-2025-11-24,compact-2026-01-12,files-api-2025-04-14"

// DefaultMaxTokens is the max_tokens value sent to the Anthropic Messages API,
// matching bifrost's AnthropicDefaultMaxTokens convention.
const DefaultMaxTokens = 4096

// DefaultClientID is the Anthropic OAuth client ID the Claude Code CLI uses.
const DefaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// DefaultModelMapping maps Anthropic model names to Claude backend model slugs.
// The Claude backend accepts the standard Anthropic model IDs directly, so this
// is largely identity; it exists for alias/override flexibility.
var DefaultModelMapping = map[string]string{
	"claude-opus-4":        "claude-opus-4",
	"claude-opus-4-1":      "claude-opus-4-1",
	"claude-opus-4-5":      "claude-opus-4-5",
	"claude-opus-4-8":      "claude-opus-4-8",
	"claude-sonnet-4-5":    "claude-sonnet-4-5",
	"claude-sonnet-4":      "claude-sonnet-4",
	"claude-haiku-4-5":     "claude-haiku-4-5",
	"claude-haiku-3-5":     "claude-haiku-3-5",
	"claude-3-5-sonnet":    "claude-sonnet-4-5",
	"claude-3-7-sonnet":    "claude-sonnet-4-5",
	"opus":                 "claude-opus-4",
	"sonnet":               "claude-sonnet-4",
	"haiku":                "claude-haiku-4-5",
	"fable":                "claude-haiku-4-5",
}

// ApplyDefaults fills unset fields with their documented defaults. It is safe
// to call on a zero-value or newly-parsed PluginConfig and returns the same
// pointer for chaining.
func (c *PluginConfig) ApplyDefaults() *PluginConfig {
	if c.APIBase == "" {
		c.APIBase = DefaultAPIBase
	}
	if c.ClientID == "" {
		c.ClientID = DefaultClientID
	}
	if c.BetaFlags == "" {
		c.BetaFlags = DefaultBetaFlags
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = DefaultMaxTokens
	}
	return c
}

// Validate checks that the config has a usable token/credential source and that
// any configured model list is set sensibly. It returns an error for
// configuration that would prevent the plugin from running. A permissive empty
// Models list is allowed (that state means "intercept all mapped models").
func (c *PluginConfig) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if c.AccessToken == "" && c.CredentialsFile == "" && !c.UseKeychain && c.TokenFile == "" {
		return fmt.Errorf("no credential source configured: set access_token, credentials_file, use_keychain, or token_file")
	}
	return nil
}

// parseConfig converts a generic map to PluginConfig. Field defaults are NOT
// applied here; call ApplyDefaults (via Init or RedactConfig) to fill them.
func parseConfig(cfgMap map[string]any) (*PluginConfig, error) {
	cfg := &PluginConfig{}

	if v, ok := cfgMap["access_token"].(string); ok {
		cfg.AccessToken = v
	}
	if v, ok := cfgMap["account_id"].(string); ok {
		cfg.AccountID = v
	}
	if v, ok := cfgMap["credentials_file"].(string); ok {
		cfg.CredentialsFile = v
	}
	if v, ok := cfgMap["use_keychain"].(bool); ok {
		cfg.UseKeychain = v
	}
	if v, ok := cfgMap["token_file"].(string); ok {
		cfg.TokenFile = v
	}
	if v, ok := cfgMap["client_id"].(string); ok {
		cfg.ClientID = v
	}
	if v, ok := cfgMap["beta_flags"].(string); ok {
		cfg.BetaFlags = v
	}
	if v, ok := cfgMap["max_tokens"].(float64); ok {
		cfg.MaxTokens = int(v)
	}
	if v, ok := cfgMap["api_base"].(string); ok {
		cfg.APIBase = v
	}
	if v, ok := cfgMap["max_retries"].(float64); ok {
		cfg.MaxRetries = int(v)
	}

	// Parse models list
	if modelsRaw, ok := cfgMap["models"].([]any); ok {
		cfg.Models = make([]string, 0, len(modelsRaw))
		for _, m := range modelsRaw {
			if s, ok := m.(string); ok {
				cfg.Models = append(cfg.Models, s)
			}
		}
	}

	// Parse model mapping
	if mappingRaw, ok := cfgMap["model_mapping"].(map[string]any); ok {
		cfg.ModelMapping = make(map[string]string, len(mappingRaw))
		for k, v := range mappingRaw {
			if s, ok := v.(string); ok {
				cfg.ModelMapping[k] = s
			}
		}
	}

	return cfg, nil
}

// MarshalConfigForStorage converts the live config to DB-storage format.
func MarshalConfigForStorage(config map[string]any) (map[string]any, error) {
	return config, nil
}

// configField describes a single plugin configuration option, used to build the
// canonical (visually renderable) config view and the DescribeConfig export.
type configField struct {
	Key         string // JSON key
	Kind        string // "secret", "path", "string", "bool", "int", "list", "map"
	Description string // short human-readable description for UI tooltips/labels
	Default     any    // effective default when the field is unset (nil if none)
}

// describeConfigFields returns the ordered list of configuration options exposed
// by this plugin. This is the single source of truth for the UI-renderable view;
// it mirrors the PluginConfig struct and its Default* constants.
func describeConfigFields() []configField {
	return []configField{
		{Key: "access_token", Kind: "secret", Description: "Claude Code OAuth access token. Takes precedence over credential-import sources. If unset, the plugin surfaces an authorization URL in logs/status to sign in.", Default: ""},
		{Key: "account_id", Kind: "string", Description: "Anthropic organization/account ID (ORG-Id header).", Default: ""},
		{Key: "credentials_file", Kind: "path", Description: "Path to a JSON file containing Claude OAuth credentials (~/.claude/.credentials.json).", Default: ""},
		{Key: "use_keychain", Kind: "bool", Description: "Import the credential from macOS Keychain (service 'Claude Code-credentials').", Default: true},
		{Key: "token_file", Kind: "path", Description: "Path where refreshed tokens are persisted. Default ~/.bifrost/claude-auth.json.", Default: ""},
		{Key: "client_id", Kind: "string", Description: "Anthropic OAuth client ID used for refresh requests.", Default: DefaultClientID},
		{Key: "beta_flags", Kind: "string", Description: "Comma-separated anthropic-beta header value override.", Default: DefaultBetaFlags},
		{Key: "max_tokens", Kind: "int", Description: "max_tokens value sent to the Anthropic Messages API.", Default: DefaultMaxTokens},
		{Key: "models", Kind: "list", Description: "Anthropic model names to intercept and reroute through the Claude Code subscription.", Default: []string{}},
		{Key: "model_mapping", Kind: "map", Description: "Overrides for the default Anthropic→Claude slug mapping.", Default: DefaultModelMapping},
		{Key: "api_base", Kind: "string", Description: "Anthropic API base URL.", Default: DefaultAPIBase},
		{Key: "max_retries", Kind: "int", Description: "Number of retries for failed HTTP calls.", Default: 0},
	}
}

// RedactConfig builds the API-response (UI-visible) configuration view.
//
// It returns a canonical map containing every supported option with its effective
// value applied (defaults filled in from the Default* constants), with secret
// values masked. This lets the Bifrost UI render a complete configuration form
// even when the user has not set every field.
func RedactConfig(config map[string]any) (map[string]any, error) {
	cfg, err := parseConfig(config)
	if err != nil {
		return nil, err
	}
	cfg.ApplyDefaults()
	return redactedView(cfg), nil
}

// redactedView converts a parsed PluginConfig into the canonical, redacted
// key/value map for API responses. Callers should have applied defaults first.
func redactedView(cfg *PluginConfig) map[string]any {
	if cfg == nil {
		cfg = &PluginConfig{}
	}
	models := cfg.Models
	if models == nil {
		models = []string{}
	}
	mapping := cfg.ModelMapping
	if mapping == nil {
		mapping = map[string]string{}
	}

	return map[string]any{
		"access_token":     redactSecret(cfg.AccessToken),
		"account_id":       cfg.AccountID,
		"credentials_file": cfg.CredentialsFile,
		"use_keychain":     cfg.UseKeychain,
		"token_file":       cfg.TokenFile,
		"client_id":        firstNonEmpty(cfg.ClientID, DefaultClientID),
		"beta_flags":       firstNonEmpty(cfg.BetaFlags, DefaultBetaFlags),
		"max_tokens":       firstNonZero(cfg.MaxTokens, DefaultMaxTokens),
		"models":           models,
		"model_mapping":    mapping,
		"api_base":         firstNonEmpty(cfg.APIBase, DefaultAPIBase),
		"max_retries":      cfg.MaxRetries,
	}
}

// DescribeConfig returns a human/machine readable description of every
// configuration option this plugin supports. It is an additive export (not part
// of the bifrost loader contract) intended for UIs that want to render a
// configuration form with labels, types, defaults, and descriptions.
func DescribeConfig() map[string]any {
	fields := describeConfigFields()
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		out[f.Key] = map[string]any{
			"kind":        f.Kind,
			"description": f.Description,
			"default":     f.Default,
		}
	}

	// Auth guidance field reflecting the current authentication state and the
	// authorization URL surfaced in logs/status when not authenticated.
	if status := GetAuthStatus(); status != nil {
		out["_auth"] = map[string]any{
			"kind":        "status",
			"description": "Authorization state. When not authenticated the plugin logs the Claude authorization URL and code#state to paste back; complete it with 'claude-sub-login --login' or by importing from the Claude Code CLI.",
			"value":       status,
		}
	}
	return out
}

// redactSecret returns "***" for a non-empty secret and "" otherwise.
func redactSecret(v string) string {
	if v == "" {
		return ""
	}
	return "***"
}

// firstNonEmpty returns v when non-empty, otherwise fallback.
func firstNonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

// firstNonZero returns v when non-zero, otherwise fallback.
func firstNonZero(v, fallback int) int {
	if v != 0 {
		return v
	}
	return fallback
}
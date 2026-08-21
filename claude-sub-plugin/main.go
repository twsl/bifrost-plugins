package main

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

const pluginName = "claude-sub"

// State held across plugin invocations.
var (
	claudeClient *ClaudeClient
	pluginConfig *PluginConfig
	tokenManager *TokenManager
	authFlow     *AuthFlow
)

// Init is called when the plugin is loaded.
func Init(config any) error {
	if config == nil {
		return fmt.Errorf("claude-sub: config is required")
	}

	cfgMap, ok := config.(map[string]any)
	if !ok {
		return fmt.Errorf("claude-sub: invalid config type: %T (expected map)", config)
	}

	cfg, err := parseConfig(cfgMap)
	if err != nil {
		return fmt.Errorf("claude-sub: invalid config: %w", err)
	}
	cfg.ApplyDefaults()

	// Initialize the token manager. Missing tokens are non-fatal: the plugin
	// loads unauthenticated and surfaces the authorization URL on demand.
	tm, err := NewTokenManager(cfg)
	if err != nil {
		tokenManager = tm
		pluginConfig = cfg
		claudeClient = NewClaudeClient(cfg, tm)
		authFlow = NewAuthFlow(tm)
		fmt.Printf("claude-sub: loaded WITHOUT auth (see plugin status for the login URL): %v\n", err)
		return nil
	}
	tokenManager = tm

	cfg.AccessToken = tm.GetAccessToken()
	pluginConfig = cfg
	claudeClient = NewClaudeClient(cfg, tm)
	authFlow = NewAuthFlow(tm)

	fmt.Printf("claude-sub: initialized (models=%v)\n", cfg.Models)
	return nil
}

// GetName returns the unique identifier for this plugin.
func GetName() string {
	return pluginName
}

// GetAuthStatus returns a JSON-friendly view of the plugin's authentication
// state, including the Claude authorization URL and instructions when not
// authenticated. This is a non-contract additive export hosts can call to
// surface the status to the user.
func GetAuthStatus() map[string]any {
	authenticated := false
	if tokenManager != nil {
		authenticated = tokenManager.Authenticated()
	}

	status := map[string]any{
		"name":          pluginName,
		"authenticated": authenticated,
	}

	if !authenticated && authFlow != nil {
		st := authFlow.ensureStarted()
		status["auth_url"] = st.AuthURL
		status["code_format"] = st.CodeFormat
		status["instructions"] = st.Instructions
	} else if !authenticated {
		status["instructions"] = "Run 'claude-sub-login --login' or configure an access_token for this plugin."
	} else {
		status["instructions"] = "Authenticated. Claude-backed requests are being proxied."
	}

	return status
}

// PreRequestHook is called once per top-level request.
func PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

// PreLLMHook intercepts Anthropic requests and short-circuits via the
// Claude Code OAuth subscription path.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if claudeClient == nil || pluginConfig == nil {
		return req, nil, nil
	}

	provider, model, _ := req.GetRequestFields()

	if provider != schemas.Anthropic {
		return req, nil, nil
	}

	if !shouldIntercept(model, pluginConfig.Models) {
		return req, nil, nil
	}

	ctx.Log(schemas.LogLevelInfo, fmt.Sprintf("claude-sub: intercepting request for model=%s", model))

	// If not authenticated, surface the authorization URL and fall through to
	// the upstream provider rather than failing the request.
	if tokenManager != nil && !tokenManager.Authenticated() {
		if authFlow != nil {
			st := authFlow.ensureStarted()
			if st.AuthURL != "" {
				ctx.Log(schemas.LogLevelWarn,
					fmt.Sprintf("claude-sub: NOT AUTHENTICATED. Please authorize to use this plugin.\n"+
						"  Open: %s\n  After signing in, copy the code#state from the browser "+
						"address bar and run 'claude-sub-login --login' (or 'claude-sub-login' to "+
						"import from the Claude Code CLI):\n  %s", st.AuthURL, st.Instructions))
			} else {
				ctx.Log(schemas.LogLevelWarn,
					fmt.Sprintf("claude-sub: NOT AUTHENTICATED. %s", st.Instructions))
			}
		} else {
			ctx.Log(schemas.LogLevelWarn,
				"claude-sub: NOT AUTHENTICATED and no auth flow available. "+
					"Run 'claude-sub-login --login' or set an access_token in the plugin config.")
		}
		// Fall through so the request still reaches the configured upstream.
		return req, nil, nil
	}

	resp, err := claudeClient.TranslateAndCall(ctx, req)
	if err != nil {
		ctx.Log(schemas.LogLevelWarn, fmt.Sprintf("claude-sub: API call failed, falling through: %v", err))
		return req, nil, nil
	}

	ctx.Log(schemas.LogLevelInfo, fmt.Sprintf("claude-sub: successfully proxied request for model=%s", model))
	return req, &schemas.LLMPluginShortCircuit{Response: resp}, nil
}

// shouldIntercept checks whether the given model should be intercepted.
func shouldIntercept(model string, models []string) bool {
	if len(models) == 0 {
		if _, ok := DefaultModelMapping[model]; ok {
			return true
		}
		return strings.HasPrefix(model, "claude")
	}
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

// PostLLMHook is called after receiving a response from the provider.
func PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}

// Cleanup is called when Bifrost shuts down.
func Cleanup() error {
	if claudeClient != nil {
		claudeClient.httpClient.CloseIdleConnections()
	}
	fmt.Println("claude-sub: cleaned up")
	return nil
}

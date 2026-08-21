package main

import (
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

const pluginName = "codex-sub"

// State held across plugin invocations.
var (
	codexClient  *CodexClient
	pluginConfig *PluginConfig
	tokenManager *TokenManager
	authFlow     *AuthFlow
)

// Init is called when the plugin is loaded.
// config contains the plugin configuration from Bifrost's config.json.
func Init(config any) error {
	if config == nil {
		return fmt.Errorf("codex-sub: config is required")
	}

	cfgMap, ok := config.(map[string]any)
	if !ok {
		return fmt.Errorf("codex-sub: invalid config type: %T (expected map)", config)
	}

	cfg, err := parseConfig(cfgMap)
	if err != nil {
		return fmt.Errorf("codex-sub: invalid config: %w", err)
	}
	cfg.ApplyDefaults()

	// Initialize the token manager. Missing tokens are non-fatal: the plugin
	// loads unauthenticated and surfaces the device-flow auth URL on demand.
	tm, err := NewTokenManager(cfg)
	if err != nil {
		// Still initialize the client so PreLLMHook can run and emit the
		// auth URL; the token manager is also kept (unauthenticated).
		tokenManager = tm
		pluginConfig = cfg
		codexClient = NewCodexClient(cfg, tm)
		authFlow = NewAuthFlow(tm)
		fmt.Printf("codex-sub: loaded WITHOUT auth (visit the plugin status for the login URL): %v\n", err)
		return nil
	}
	tokenManager = tm

	// Set the resolved access token on the config for the client
	cfg.AccessToken = tm.GetAccessToken()

	pluginConfig = cfg
	codexClient = NewCodexClient(cfg, tm)
	authFlow = NewAuthFlow(tm)

	fmt.Printf("codex-sub: initialized (models=%v)\n", cfg.Models)
	return nil
}

// GetName returns the unique identifier for this plugin.
func GetName() string {
	return pluginName
}

// GetAuthStatus returns a JSON-friendly view of the plugin's authentication
// state, including the live device-flow verification URL and one-time code
// when the plugin is not authenticated. This is a non-contract additive export
// that hosts can call to surface the status to the user.
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
		status["verification_url"] = st.VerificationURL
		status["user_code"] = st.UserCode
		status["instructions"] = st.Instructions
		status["pending"] = st.Pending
		status["completed"] = st.Completed
		status["failed"] = st.Failed
		status["error"] = st.Error
	} else if !authenticated {
		status["instructions"] = "Run 'codex-sub-login' or configure an access_token for this plugin."
	} else {
		status["instructions"] = "Authenticated. Codex-backed requests are being proxied."
	}

	return status
}

// PreRequestHook is called once per top-level request.
// This plugin doesn't participate in routing decisions.
func PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

// PreLLMHook is called before each provider request.
// If the request targets an OpenAI model we have a ChatGPT mapping for,
// we intercept and short-circuit via the ChatGPT API.
// Otherwise, we pass through to the normal provider.
func PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	// Guard: client must be initialized
	if codexClient == nil || pluginConfig == nil {
		return req, nil, nil
	}

	provider, model, _ := req.GetRequestFields()

	// Only intercept OpenAI requests
	if provider != schemas.OpenAI {
		return req, nil, nil
	}

	// Check if this model is in our interception list (or if no filter is set, intercept all mapped models)
	if !shouldIntercept(model, pluginConfig.Models) {
		return req, nil, nil
	}

	ctx.Log(schemas.LogLevelInfo, fmt.Sprintf("codex-sub: intercepting request for model=%s", model))

	// If not authenticated, surface the device-flow auth URL and fall through
	// to the upstream provider rather than failing the request.
	if tokenManager != nil && !tokenManager.Authenticated() {
		if authFlow != nil {
			st := authFlow.ensureStarted()
			ctx.Log(schemas.LogLevelWarn,
				fmt.Sprintf("codex-sub: NOT AUTHENTICATED. Please authorize to use this plugin.\n"+
					"  Open: %s\n  Enter code: %s\n  (%s)",
					st.AuthURL, st.UserCode, st.Instructions))
		} else {
			ctx.Log(schemas.LogLevelWarn,
				"codex-sub: NOT AUTHENTICATED and no auth flow available. "+
					"Run 'codex-sub-login' or set an access_token in the plugin config.")
		}
		// Fall through so the request still reaches the configured upstream.
		return req, nil, nil
	}

	// Call the Codex backend API
	resp, err := codexClient.TranslateAndCall(ctx, req)
	if err != nil {
		ctx.Log(schemas.LogLevelWarn, fmt.Sprintf("codex-sub: API call failed, falling through: %v", err))
		return req, nil, nil
	}

	ctx.Log(schemas.LogLevelInfo, fmt.Sprintf("codex-sub: successfully proxied request for model=%s", model))
	return req, &schemas.LLMPluginShortCircuit{Response: resp}, nil
}

// shouldIntercept checks whether the given model should be intercepted.
// If no models are configured, all known mapped models are intercepted.
func shouldIntercept(model string, models []string) bool {
	if len(models) == 0 {
		// No filter set: intercept if we have a mapping for this model,
		// or if the model name already looks like a codex slug.
		if _, ok := DefaultModelMapping[model]; ok {
			return true
		}
		return strings.Contains(model, "codex")
	}
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

// PostLLMHook is called after receiving a response from the provider.
// No-op for this plugin.
func PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}

// Cleanup is called when Bifrost shuts down.
func Cleanup() error {
	if codexClient != nil {
		codexClient.httpClient.CloseIdleConnections()
	}
	fmt.Println("codex-sub: cleaned up")
	return nil
}

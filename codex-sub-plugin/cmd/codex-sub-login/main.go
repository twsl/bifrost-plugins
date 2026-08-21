// codex-sub-login is a CLI tool that performs the OAuth device code login
// flow against auth.openai.com, obtaining Codex access tokens that the
// Bifrost codex-sub plugin can use.
//
// Usage:
//
//	codex-sub-login                    Run the full device code login flow
//	codex-sub-login --refresh          Refresh an existing token
//	codex-sub-login --json             Output tokens as JSON (for scripting)
//	codex-sub-login --token-file PATH  Custom token file path
//	codex-sub-login --help             Show full usage
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/twsl/bifrost/codex-sub-plugin/login"
)

const (
	ansiBold  = "\033[1m"
	ansiCyan  = "\033[36m"
	ansiGray  = "\033[90m"
	ansiReset = "\033[0m"
)

func main() {
	tokenFile := flag.String("token-file", "", "Path to the token file (default: ~/.bifrost/auth.json)")
	issuer := flag.String("issuer", login.DefaultIssuer, "OAuth issuer URL")
	clientID := flag.String("client-id", login.DefaultClientID, "OAuth client ID")
	outputJSON := flag.Bool("json", false, "Output tokens as JSON to stdout (for scripting)")
	refreshMode := flag.Bool("refresh", false, "Refresh an existing token instead of running the full login flow")
	flag.Parse()

	// Resolve token file path
	tokenFilePath := *tokenFile
	if tokenFilePath == "" {
		var err error
		tokenFilePath, err = login.DefaultTokenFilePath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}

	if *refreshMode {
		runRefresh(tokenFilePath, *issuer, *clientID, *outputJSON)
		return
	}

	runLogin(tokenFilePath, *issuer, *clientID, *outputJSON)
}

func runLogin(tokenFilePath, issuer, clientID string, outputJSON bool) {
	fmt.Fprintf(os.Stderr, "\n%sWelcome to Codex Subscription Login%s\n", ansiBold, ansiReset)
	fmt.Fprintf(os.Stderr, "%sAuthenticate using your OpenAI account to get API access tokens%s\n\n", ansiGray, ansiReset)

	// Request device code
	dc, err := login.RequestDeviceCode(issuer, clientID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error requesting device code: %v\n", err)
		os.Exit(1)
	}

	// Display instructions
	fmt.Fprintf(os.Stderr, "Follow these steps to sign in with your OpenAI account:\n\n")
	fmt.Fprintf(os.Stderr, "  1. Open this link in your browser and sign in:\n")
	fmt.Fprintf(os.Stderr, "     %s%s%s\n\n", ansiCyan, dc.VerificationURL, ansiReset)
	fmt.Fprintf(os.Stderr, "  2. Enter this one-time code (expires in 15 minutes):\n")
	fmt.Fprintf(os.Stderr, "     %s%s%s\n\n", ansiCyan, dc.UserCode, ansiReset)
	fmt.Fprintf(os.Stderr, "%sDevice codes are a common phishing target. Never share this code.%s\n\n", ansiGray, ansiReset)
	fmt.Fprintf(os.Stderr, "Waiting for you to complete authentication...\n")

	// Poll for authorization
	result, err := login.PollForToken(issuer, dc.DeviceAuthID, dc.UserCode, dc.Interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nError waiting for authorization: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "\nAuthorization received! Exchanging for tokens...\n")

	// Exchange authorization code for tokens
	baseURL := strings.TrimRight(issuer, "/")
	redirectURI := baseURL + "/deviceauth/callback"
	tokens, err := login.ExchangeCodeForTokens(issuer, clientID, redirectURI,
		result.CodeVerifier, result.AuthorizationCode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error exchanging code for tokens: %v\n", err)
		os.Exit(1)
	}

	// Save tokens to file
	if err := login.SaveTokens(tokenFilePath, tokens); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving tokens: %v\n", err)
		os.Exit(1)
	}

	// Report success
	fmt.Fprintf(os.Stderr, "\n%s✓ Login successful!%s\n", ansiBold, ansiReset)
	fmt.Fprintf(os.Stderr, "Tokens saved to: %s\n", tokenFilePath)

	// Show masked access token for verification
	maskedToken := maskToken(tokens.AccessToken)
	fmt.Fprintf(os.Stderr, "Access token: %s\n", maskedToken)

	if tokens.RefreshToken != "" {
		maskedRefresh := maskToken(tokens.RefreshToken)
		fmt.Fprintf(os.Stderr, "Refresh token: %s\n", maskedRefresh)
	}

	fmt.Fprintf(os.Stderr, "\nYou can now use the Bifrost codex-sub plugin.\n")
	fmt.Fprintf(os.Stderr, "Add the following to your Bifrost config:\n")
	fmt.Fprintf(os.Stderr, "  %s\"token_file\": \"%s\"%s\n", ansiGray, tokenFilePath, ansiReset)

	if outputJSON {
		printJSON(tokens)
	}
}

func runRefresh(tokenFilePath, issuer, clientID string, outputJSON bool) {
	fmt.Fprintf(os.Stderr, "Refreshing tokens from: %s\n", tokenFilePath)

	tokens, err := login.LoadTokens(tokenFilePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading token file: %v\n", err)
		os.Exit(1)
	}
	if tokens == nil || tokens.RefreshToken == "" {
		fmt.Fprintf(os.Stderr, "No refresh token found. Run 'codex-sub-login' to authenticate first.\n")
		os.Exit(1)
	}

	newTokens, err := login.RefreshTokens(issuer, clientID, tokens.RefreshToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error refreshing tokens: %v\n", err)
		os.Exit(1)
	}

	// Preserve ID token if not returned by refresh
	if newTokens.IDToken == "" {
		newTokens.IDToken = tokens.IDToken
	}
	// Preserve refresh token if not rotated
	if newTokens.RefreshToken == "" {
		newTokens.RefreshToken = tokens.RefreshToken
	}

	if err := login.SaveTokens(tokenFilePath, newTokens); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving refreshed tokens: %v\n", err)
		os.Exit(1)
	}

	maskedToken := maskToken(newTokens.AccessToken)
	fmt.Fprintf(os.Stderr, "%s✓ Token refreshed!%s\n", ansiBold, ansiReset)
	fmt.Fprintf(os.Stderr, "New access token: %s\n", maskedToken)

	if outputJSON {
		printJSON(newTokens)
	}
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return strings.Repeat("*", len(token))
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "Error encoding JSON: %v\n", err)
		os.Exit(1)
	}
}

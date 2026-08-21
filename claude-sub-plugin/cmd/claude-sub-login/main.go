// claude-sub-login imports Claude Code OAuth credentials (from the macOS
// Keychain or ~/.claude/.credentials.json) and makes them available to the
// Bifrost claude-sub plugin. It can also run the Claude browser authorization
// flow directly (the Claude Code authorization-code + PKCE equivalent of a
// device flow):
//
//  1. Print an authorize URL for the user to open.
//  2. Wait for the user to paste back the "code#state" from the browser.
//  3. Exchange the code for tokens and write them to the plugin token file.
//
// Usage:
//
//	claude-sub-login                    Import existing Claude Code credentials
//	claude-sub-login --login            Run the browser authorization flow
//	claude-sub-login --refresh          Refresh an imported token
//	claude-sub-login --json             Print tokens as JSON
//	claude-sub-login --token-file T     Override the plugin token file
//	claude-sub-login --credentials F    Import from a specific credentials file
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/twsl/bifrost/claude-sub-plugin/login"
)

const defaultClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

func main() {
	tokenFile := flag.String("token-file", "", "Plugin token file (default ~/.bifrost/claude-auth.json)")
	credentialsFile := flag.String("credentials", "", "Claude credentials JSON file (default ~/.claude/.credentials.json)")
	clientID := flag.String("client-id", defaultClientID, "Anthropic OAuth client ID")
	outputJSON := flag.Bool("json", false, "Output tokens as JSON")
	refresh := flag.Bool("refresh", false, "Refresh an existing token")
	loginFlow := flag.Bool("login", false, "Run the Claude browser authorization flow (prints a URL, wait for the pasted code)")
	flag.Parse()

	tf := *tokenFile
	if tf == "" {
		p, err := login.DefaultTokenFilePath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		tf = p
	}

	if *loginFlow {
		if *refresh {
			fmt.Fprintln(os.Stderr, "Error: --login and --refresh are mutually exclusive.")
			os.Exit(1)
		}
		runBrowserLogin(tf, *clientID, *outputJSON)
		return
	}

	// Import existing credentials (unless refreshing).
	tokens, ok := login.Import(*credentialsFile, true)
	if !ok && *refresh {
		// Fall back to the persisted token file when refreshing.
		if data, err := os.ReadFile(tf); err == nil {
			var t login.Tokens
			if json.Unmarshal(data, &t) == nil && t.RefreshToken != "" {
				tokens = t
				ok = true
			}
		}
	}

	if !ok {
		fmt.Fprintf(os.Stderr, "Error: no Claude Code credentials found.\n")
		fmt.Fprintf(os.Stderr, "Run 'claude-sub-login --login' to authorize via the browser,\n")
		fmt.Fprintf(os.Stderr, "or log in with the official Claude Code CLI first\n")
		fmt.Fprintf(os.Stderr, "(it stores credentials in ~/.claude/.credentials.json or the macOS Keychain).\n")
		os.Exit(1)
	}

	if *refresh {
		if tokens.RefreshToken == "" {
			fmt.Fprintf(os.Stderr, "Error: no refresh token available.\n")
			os.Exit(1)
		}
		newTokens, err := login.RefreshTokens(*clientID, tokens.RefreshToken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error refreshing: %v\n", err)
			os.Exit(1)
		}
		if newTokens.RefreshToken == "" {
			newTokens.RefreshToken = tokens.RefreshToken
		}
		if err := save(tf, newTokens); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving tokens: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "✓ Token refreshed (access token: %s)\n", maskToken(newTokens.AccessToken))
		if *outputJSON {
			printJSON(newTokens)
		}
		return
	}

	if err := save(tf, tokens); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving tokens: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "✓ Imported Claude Code credentials\n")
	fmt.Fprintf(os.Stderr, "Access token: %s\n", maskToken(tokens.AccessToken))
	fmt.Fprintf(os.Stderr, "Saved to: %s\n", tf)

	if *outputJSON {
		printJSON(tokens)
	}
}

// runBrowserLogin drives the Claude authorization-code + PKCE flow, the
// closest Claude equivalent to a device flow (the protocol has no RFC-8628
// grant, so the user pastes the code back from the console callback instead).
func runBrowserLogin(tokenFile, clientID string, outputJSON bool) {
	pkce, err := login.GeneratePKCE()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating PKCE: %v\n", err)
		os.Exit(1)
	}

	state := "claude-sub-login-" + randHex(8)
	authURL, err := login.AuthorizeURL(clientID, state, pkce)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building authorize URL: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "== Claude browser authorization ==")
	fmt.Fprintln(os.Stderr, "Open this URL in your browser and sign in:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %s\n", authURL)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "After you approve, the browser redirects to the Claude console")
	fmt.Fprintf(os.Stderr, "callback (redirect_uri=%s). Copy the \"code#state\" value from the address bar\n", login.RedirectURI)
	fmt.Fprintf(os.Stderr, "and paste it below (it starts with %q):\n", state)

	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stderr, "Code#state> ")
	pasted, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading code: %v\n", err)
		os.Exit(1)
	}

	code, _, perr := login.ParseAuthorizationCodeInput(pasted, state)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", perr)
		fmt.Fprintln(os.Stderr, "Expected the value like \"<code>#<state>\" from the browser address bar.")
		os.Exit(1)
	}

	tokens, err := login.ExchangeCodeDefault(clientID, state, code, pkce.Verifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error exchanging code: %v\n", err)
		os.Exit(1)
	}

	if err := save(tokenFile, tokens); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving tokens: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "✓ Authenticated with Claude\n")
	fmt.Fprintf(os.Stderr, "Access token: %s\n", maskToken(tokens.AccessToken))
	if tokens.AccountEmail != "" {
		fmt.Fprintf(os.Stderr, "Account:     %s\n", tokens.AccountEmail)
	}
	fmt.Fprintf(os.Stderr, "Saved to:    %s\n", tokenFile)

	if outputJSON {
		printJSON(tokens)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b)
}

func save(path string, tok login.Tokens) error {
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return "***"
	}
	return token[:4] + "***" + token[len(token)-4:]
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

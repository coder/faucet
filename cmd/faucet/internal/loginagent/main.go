// Command loginagent is the coding agent of faucet's end-to-end tests. It
// logs in to an MCP endpoint of faucet the way go-sdk's authorization code
// handler does, from the endpoint's URL and the URL of its client ID
// metadata document alone, plays the person who consents in a browser,
// calls wait_for_github_events once and prints the result as JSON. With
// -refresh it then refreshes its token, calls the tool again on the same
// session, and presents the refresh token that the refresh replaced, which
// faucet trades again within 30 seconds of its first use. With
// -write-cert it writes the TLS certificate that the tests serve the
// documents with, and exits. It reads no environment variable.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/xerrors"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "loginagent:", err)
		os.Exit(1)
	}
}

// outcome is what the agent prints.
type outcome struct {
	// Result is the answer of the first wait_for_github_events call.
	Result *mcp.CallToolResult `json:"result"`
	// Refreshed is the answer of the call after the refresh, with -refresh.
	Refreshed *mcp.CallToolResult `json:"refreshed,omitempty"`
	// Reused is what the token endpoint answered to the refresh token that
	// the refresh replaced, with -refresh: "issued", or its OAuth error
	// code.
	Reused string `json:"reused,omitempty"`
	// Logins counts the consent pages that the person went through. go-sdk
	// logs in again when a refresh fails, so a refresh that works leaves it
	// at 1.
	Logins int `json:"logins"`
	// Tokens are the faucet tokens that the agent held, for the tests to
	// look for in faucet's output.
	Tokens []string `json:"tokens"`
}

func run() error {
	endpoint := flag.String("url", "", "the MCP endpoint, such as https://faucet.example.com/github/mcp")
	clientID := flag.String("client-id", "", "the https URL of the agent's client ID metadata document")
	redirectURI := flag.String("redirect-uri", "http://127.0.0.1:53682/callback", "where the login sends the browser back to")
	repo := flag.String("repo", "", "the repository to wait on, owner/name")
	types := flag.String("types", "", "the event types to wait for, comma separated")
	cursor := flag.Int64("cursor", 0, "the cursor to wait after")
	timeout := flag.Int("timeout", 1, "how long a wait lasts, in seconds")
	refresh := flag.Bool("refresh", false, "after the first call, refresh the token, call again and present the replaced refresh token")
	certDir := flag.String("write-cert", "", "write cert.pem and key.pem for 127.0.0.1 and localhost to this directory, and exit")
	flag.Parse()
	if *certDir != "" {
		return writeCert(*certDir)
	}
	if *endpoint == "" || *clientID == "" || *repo == "" || *types == "" {
		return xerrors.New("-url, -client-id, -repo and -types are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Duration(*timeout)*time.Second+30*time.Second)
	defer cancel()
	person, err := newPerson(*redirectURI)
	if err != nil {
		return err
	}
	tokens := &refresher{}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &auth.ClientIDMetadataDocumentConfig{URL: *clientID},
		RedirectURL:                    *redirectURI,
		AuthorizationCodeFetcher:       person.login,
		NewTokenSource:                 tokens.start,
	})
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "faucet-loginagent", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *endpoint, OAuthHandler: handler, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return xerrors.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	wait := &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: map[string]any{
		"repo": *repo, "types": strings.Split(*types, ","), "cursor": *cursor, "timeout_s": *timeout,
	}}

	var out outcome
	if out.Result, err = session.CallTool(ctx, wait); err != nil {
		return xerrors.Errorf("call wait_for_github_events: %w", err)
	}
	if *refresh {
		replaced, err := tokens.expire()
		if err != nil {
			return err
		}
		if out.Refreshed, err = session.CallTool(ctx, wait); err != nil {
			return xerrors.Errorf("call wait_for_github_events after the refresh: %w", err)
		}
		if out.Reused, err = tokens.present(replaced); err != nil {
			return err
		}
	}
	out.Logins, out.Tokens = person.logins, tokens.seen()
	return json.NewEncoder(os.Stdout).Encode(out)
}

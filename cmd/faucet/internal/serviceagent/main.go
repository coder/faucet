// Command serviceagent is the service agent of faucet's end-to-end tests. It
// logs in to an MCP endpoint of faucet the way go-sdk's client credentials
// handler does, from the endpoint's URL, a client ID and the client secret
// alone, calls wait_for_github_events once and prints the result as JSON.
// The secret comes from FAUCET_CLIENT_SECRET, so it is not in the process
// list.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth/extauth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/xerrors"
)

const envSecret = "FAUCET_CLIENT_SECRET" //nolint:gosec // The name of an environment variable, not a credential.

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "serviceagent:", err)
		os.Exit(1)
	}
}

func run() error {
	endpoint := flag.String("url", "", "the MCP endpoint, such as https://faucet.example.com/github/mcp")
	clientID := flag.String("client-id", "", "the client ID")
	repo := flag.String("repo", "", "the repository to wait on, owner/name")
	types := flag.String("types", "", "the event types to wait for, comma separated")
	cursor := flag.Int64("cursor", 0, "the cursor to wait after")
	timeout := flag.Int("timeout", 1, "how long the wait lasts, in seconds")
	flag.Parse()
	secret := os.Getenv(envSecret)
	if *endpoint == "" || *clientID == "" || *repo == "" || *types == "" || secret == "" {
		return xerrors.Errorf("-url, -client-id, -repo, -types and %s are required", envSecret)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second+30*time.Second)
	defer cancel()
	handler, err := extauth.NewClientCredentialsHandler(&extauth.ClientCredentialsHandlerConfig{
		Credentials: &oauthex.ClientCredentials{ClientID: *clientID, ClientSecretAuth: &oauthex.ClientSecretAuth{ClientSecret: secret}},
	})
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "faucet-serviceagent", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *endpoint, OAuthHandler: handler, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return xerrors.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_for_github_events", Arguments: map[string]any{
		"repo": *repo, "types": strings.Split(*types, ","), "cursor": *cursor, "timeout_s": *timeout,
	}})
	if err != nil {
		return xerrors.Errorf("call wait_for_github_events: %w", err)
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

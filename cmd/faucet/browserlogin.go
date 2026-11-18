package main

import (
	"net/http"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/faucet/faucet"
	"github.com/coder/faucet/faucet/oauth"
)

const (
	envGitHubAppClientID     = "FAUCET_GITHUB_APP_CLIENT_ID"
	envGitHubAppClientSecret = "FAUCET_GITHUB_APP_CLIENT_SECRET" //nolint:gosec // The name of an environment variable, not a credential.
	envTokenKey              = "FAUCET_TOKEN_KEY"                //nolint:gosec // The name of an environment variable, not a credential.
	// envGitHubURL is GitHub's web host, which serves the App's authorize
	// page and token endpoint.
	envGitHubURL     = "FAUCET_GITHUB_URL"
	defaultGitHubURL = "https://github.com"
)

// loadBrowserLoginConfig reads the settings of the browser login, in which
// a user logs in at GitHub through faucet's GitHub App. With none of them
// set the login is off. With some set, every one that is needed must be
// set, so a typo does not turn the login off, and the public URL must be
// set, because the login server lives at it. No error names a value, because
// two of them are secrets.
func loadBrowserLoginConfig(getenv func(string) string, cfg *config) error {
	clientID, clientSecret, rawKeys, githubURL := getenv(envGitHubAppClientID), getenv(envGitHubAppClientSecret), getenv(envTokenKey), getenv(envGitHubURL)
	if clientID == "" && clientSecret == "" && rawKeys == "" && githubURL == "" {
		return nil
	}
	var missing []string
	for _, v := range []struct{ name, value string }{{envGitHubAppClientID, clientID}, {envGitHubAppClientSecret, clientSecret}, {envTokenKey, rawKeys}} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if cfg.publicURL == nil {
		missing = append(missing, envPublicURL)
	}
	if len(missing) > 0 {
		return xerrors.Errorf("the browser login needs %s", strings.Join(missing, ", "))
	}
	keys, err := oauth.ParseTokenKeys(rawKeys)
	if err != nil {
		return xerrors.Errorf("%s: %w", envTokenKey, err)
	}
	if githubURL == "" {
		githubURL = defaultGitHubURL
	}
	login := &oauth.BrowserLogin{
		GitHub: oauth.GitHubApp{
			ClientID: clientID, ClientSecret: clientSecret, WebURL: githubURL, APIURL: cfg.githubAPI,
			Client: &http.Client{Timeout: githubTimeout},
		},
		Keys:      keys,
		Documents: faucet.HTTPClientOptions{AllowInsecureLoopback: allowInsecureLoopback},
	}
	if err := login.GitHub.Validate(); err != nil {
		return xerrors.Errorf("%s: %w", envGitHubURL, err)
	}
	cfg.browserLogin = login
	return nil
}

package oauth

import "net/http"

// authorizeHandler serves the authorization endpoint of the browser login.
func (s *Server) authorizeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not built yet", http.StatusNotImplemented)
	})
}

// GitHubCallbackHandler serves GitHubCallbackPath, where GitHub sends the
// browser back to after the user logged in there.
func (s *Server) GitHubCallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not built yet", http.StatusNotImplemented)
	})
}

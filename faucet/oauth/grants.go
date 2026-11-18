package oauth

import (
	"net/http"
	"net/url"

	"golang.org/x/xerrors"
)

// authorizationCode is the authorization_code grant of a browser login.
func (s *Server) authorizationCode(_ *http.Request, _ url.Values) (tokenResponse, error) {
	return tokenResponse{}, xerrors.New("not built yet")
}

// refreshToken is the refresh_token grant of a browser login.
func (s *Server) refreshToken(_ *http.Request, _ url.Values) (tokenResponse, error) {
	return tokenResponse{}, xerrors.New("not built yet")
}

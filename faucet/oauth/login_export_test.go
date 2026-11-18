package oauth

// LoginOpenTokens opens both sealed tokens of c.
func LoginOpenTokens(keys TokenKeys, c Connection) (access, refresh string, err error) {
	if access, err = keys.accessToken(c); err != nil {
		return "", "", err
	}
	refresh, err = keys.refreshToken(c)
	return access, refresh, err
}

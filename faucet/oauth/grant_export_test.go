package oauth

// GrantSeal seals the GitHub tokens of c under the first key of keys, as the
// callback of a login does.
func GrantSeal(keys TokenKeys, c *Connection, access, refresh string) error {
	return keys.seal(c, access, refresh)
}

// GrantOpen opens both GitHub tokens of c.
func GrantOpen(keys TokenKeys, c Connection) (access, refresh string, err error) {
	if access, err = keys.accessToken(c); err != nil {
		return "", "", err
	}
	refresh, err = keys.refreshToken(c)
	return access, refresh, err
}

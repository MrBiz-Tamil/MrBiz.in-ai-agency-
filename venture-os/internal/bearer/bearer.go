// Package bearer reads the credential out of an Authorization header, once, for every service that does so.
package bearer

import "strings"

// Token returns the credential of an "Authorization: Bearer <token>" header. The scheme name is case-insensitive
// (RFC 7235 section 2.1: "bearer", "BEARER" and "Bearer" are the same scheme), so a client that sends a different case is
// not refused for it. ok is false for any other scheme, a missing header, or an empty credential.
func Token(header string) (token string, ok bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(rest)
	return token, token != ""
}

package auth

import (
	"encoding/base64"
	"strings"
)

// NewSessionToken binds the token's stored preimage to the operator's transport
// scope. Changing the prefix of a token changes its database ID, not its scope.
// Empty scope preserves the existing private-mode token format.
func NewSessionToken(scope string) (string, error) {
	raw, err := NewToken()
	if err != nil {
		return "", err
	}
	return sessionTokenPrefix(scope) + raw, nil
}

// SessionTokenMatchesScope is required before looking up a browser token. In
// particular, private mode must not accept a whole public token as a raw token.
func SessionTokenMatchesScope(token, scope string) bool {
	raw, ok := strings.CutPrefix(token, sessionTokenPrefix(scope))
	if !ok || len(raw) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	return err == nil && len(decoded) == tokenBytes && base64.RawURLEncoding.EncodeToString(decoded) == raw
}

func sessionTokenPrefix(scope string) string {
	if scope == "" {
		return ""
	}
	return "v1." + HashToken("buntzen-public-session-v1\x00"+scope) + "."
}

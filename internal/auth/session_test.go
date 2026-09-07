package auth

import "testing"

func TestSessionTokenScopeAndCanonicalRepresentation(t *testing.T) {
	scopes := []string{"", "https://a.example", "https://b.example"}
	for _, issuer := range scopes {
		token, err := NewSessionToken(issuer)
		if err != nil {
			t.Fatal(err)
		}
		for _, reader := range scopes {
			if got := SessionTokenMatchesScope(token, reader); got != (issuer == reader) {
				t.Fatalf("issuer=%q reader=%q accepted=%v", issuer, reader, got)
			}
		}
		for _, malformed := range []string{" " + token, token + "=", token + "\n", token[:len(token)-1], "v0." + token} {
			if SessionTokenMatchesScope(malformed, issuer) {
				t.Fatal("noncanonical token accepted")
			}
		}
	}
}

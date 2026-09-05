package auth

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	encoded, err := HashPassword("this is a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword(encoded, "this is a sufficiently long password")
	if err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(encoded, "not the password")
	if err != nil || ok {
		t.Fatalf("verify wrong password: ok=%v err=%v", ok, err)
	}
}

func TestNormalizeUsernameIsCaseInsensitiveAndStrict(t *testing.T) {
	normalized, err := NormalizeUsername("  Owner+Bot@Example.Test  ")
	if err != nil {
		t.Fatal(err)
	}
	if normalized != "owner+bot@example.test" {
		t.Fatalf("normalized username = %q", normalized)
	}
	for _, invalid := range []string{"", "spaces are not usernames", "emoji-☃"} {
		if _, err := NormalizeUsername(invalid); err == nil {
			t.Fatalf("invalid username %q was accepted", invalid)
		}
	}
}

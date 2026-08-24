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

func TestArgon2WorkHasAProcessWideMemoryBound(t *testing.T) {
	if got, want := cap(passwordWork), 2; got != want {
		t.Fatalf("password work capacity = %d, want %d", got, want)
	}
}

func TestTokenHashIsStableAndDoesNotExposeToken(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	first := HashToken(token)
	if first != HashToken(token) || first == token {
		t.Fatal("unexpected token hash")
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

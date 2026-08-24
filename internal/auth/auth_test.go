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

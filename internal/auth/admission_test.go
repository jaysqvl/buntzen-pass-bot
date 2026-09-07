package auth

import (
	"testing"
	"time"
)

func TestPasswordHashRejectsExcessWorkWithoutQueueing(t *testing.T) {
	for i := 0; i < cap(passwordWork); i++ {
		passwordWork <- struct{}{}
	}
	done := make(chan error, 1)
	go func() { _, err := HashPassword("a long test password"); done <- err }()
	var got error
	select {
	case got = <-done:
		for i := 0; i < cap(passwordWork); i++ {
			<-passwordWork
		}
	case <-time.After(time.Second):
		for i := 0; i < cap(passwordWork); i++ {
			<-passwordWork
		}
		<-done
		t.Fatal("excess password hash queued behind occupied slots")
	}
	if got == nil {
		t.Fatal("hash admitted while all slots were occupied")
	}
	if _, err := HashPassword("a long test password"); err != nil {
		t.Fatalf("capacity not reusable: %v", err)
	}
}

func TestAllPasswordChecksReturnBusyWithoutQueueing(t *testing.T) {
	encoded, err := HashPassword("a long test password")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"real account", func() error { _, err := VerifyPassword(encoded, "wrong password"); return err }},
		{"unknown account", func() error { return EqualizePasswordCheck("wrong password") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < cap(passwordWork); i++ {
				passwordWork <- struct{}{}
			}
			defer func() {
				for i := 0; i < cap(passwordWork); i++ {
					<-passwordWork
				}
			}()
			if err := tc.call(); err != ErrBusy {
				t.Fatalf("overload = %v", err)
			}
		})
	}
}

func TestSetupTokenFormat(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSetupToken(token); err != nil {
		t.Fatalf("generated token rejected: %v", err)
	}
	for _, value := range []string{"", "short", token + "=", token[:42], token + "A", token + "\n"} {
		if err := ValidateSetupToken(value); err == nil {
			t.Fatalf("invalid encoding accepted (length %d)", len(value))
		}
	}
}

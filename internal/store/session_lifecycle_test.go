package store

import (
	"context"
	"errors"
	"github.com/jaysqvl/buntzen-pass-bot/internal/auth"
	"testing"
	"time"
)

func TestIdleSessionCannotBeReadTouchedOrResurrected(t *testing.T) {
	database := ownedTestStore(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	ctx := context.Background()
	session, err := database.NewSession(ctx, testUserID, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Minute)
	if _, err := database.GetSession(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	if err := database.TouchSession(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if _, err := database.GetSession(ctx, session.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("idle session readable: %v", err)
	}
	if err := database.TouchSession(ctx, session.Token); !errors.Is(err, ErrNotFound) {
		t.Errorf("idle session revived by touch: %v", err)
	}
	if n, err := database.PurgeExpiredSessions(ctx); err != nil || n != 1 {
		t.Fatalf("purge idle = %d %v", n, err)
	}
}

func TestSessionReadsDoNotExtendIdleOrAbsoluteLifetime(t *testing.T) {
	database := ownedTestStore(t)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	database.now = func() time.Time { return now }
	ctx := context.Background()
	session, err := database.NewSession(ctx, testUserID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		now = now.Add(10 * time.Minute)
		if _, err := database.GetSession(ctx, session.Token); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(10 * time.Minute)
	if _, err := database.GetSession(ctx, session.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reads prolonged idle lifetime: %v", err)
	}
	session, err = database.NewSession(ctx, testUserID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(9 * time.Minute)
	if err := database.TouchSession(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := database.GetSession(ctx, session.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch prolonged absolute lifetime: %v", err)
	}
}

func TestSessionScopeCannotBeChangedByRewritingTokenPrefix(t *testing.T) {
	database := ownedTestStore(t)
	ctx := context.Background()
	_, issued, ok, err := database.AuthenticateAndCreateSessionInScope(ctx, "test-admin", "a strong test password", time.Hour, "https://a.example")
	if err != nil || !ok {
		t.Fatalf("issue session: %v %v", ok, err)
	}
	template, err := auth.NewSessionToken("https://b.example")
	if err != nil {
		t.Fatal(err)
	}
	forged := template[:len(template)-43] + issued.Token[len(issued.Token)-43:]
	if !auth.SessionTokenMatchesScope(forged, "https://b.example") {
		t.Fatal("test did not construct a syntactically valid scope substitution")
	}
	if _, err := database.GetSession(ctx, forged); !errors.Is(err, ErrNotFound) {
		t.Fatalf("altered token found: %v", err)
	}
	if _, err := database.GetSession(ctx, issued.Token); err != nil {
		t.Fatalf("unaltered token rejected: %v", err)
	}
}

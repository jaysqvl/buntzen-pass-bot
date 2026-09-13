package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
)

func TestChangeUsernamePreservesAccountOwnershipAndSessions(t *testing.T) {
	ctx := context.Background()
	for _, role := range []model.UserRole{model.RoleAdmin, model.RoleMember} {
		t.Run(string(role), func(t *testing.T) {
			database := testStore(t)
			admin, err := database.SetupAdmin(ctx, "owner", testAdminPassword)
			if err != nil {
				t.Fatal(err)
			}
			tc := struct {
				user     model.User
				password string
			}{admin, testAdminPassword}
			if role == model.RoleMember {
				tc.user, err = database.CreateMember(ctx, CreateUserInput{Username: "member", Password: testMemberPassword})
				if err != nil {
					t.Fatal(err)
				}
				tc.password = testMemberPassword
			}
			owned := database.ForUser(tc.user.ID)
			source, err := owned.CreateOTPSource(ctx, OTPSourceInput{
				Name: "Existing inbox", Provider: model.OTPProviderBlueBubbles,
				Identity: "http://messages.example:1234", ProviderConfig: map[string]string{"password": "synthetic-inbox-password"},
			})
			if err != nil {
				t.Fatal(err)
			}
			profile, err := owned.CreateProfile(ctx, ProfileInput{
				Name: "Existing profile", OTPSourceID: source.ID, LoginProbeURL: "https://example.test/login",
				DefaultVehicle: "Example Vehicle", Headless: true, DefaultTimeoutMS: 15_000, Enabled: true,
				Credentials: &model.ProfileCredentials{Phone: "5559876543"},
			})
			if err != nil {
				t.Fatal(err)
			}
			session, err := database.NewSession(ctx, tc.user.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			newName := "Renamed." + tc.user.Username
			if changed, err := database.ChangeUsername(ctx, tc.user.ID, tc.password, "  "+newName+"  "); err != nil || !changed {
				t.Fatalf("rename changed=%v err=%v", changed, err)
			}
			user, ok, err := database.AuthenticateUser(ctx, newName, tc.password)
			if err != nil || !ok || user.ID != tc.user.ID || user.Username != newName || user.Role != tc.user.Role || user.Status != tc.user.Status || user.MustChangePassword != tc.user.MustChangePassword || !user.CreatedAt.Equal(tc.user.CreatedAt) {
				t.Fatalf("renamed login user=%+v ok=%v err=%v", user, ok, err)
			}
			if _, ok, err := database.AuthenticateUser(ctx, tc.user.Username, tc.password); err != nil || ok {
				t.Fatalf("old username login ok=%v err=%v", ok, err)
			}
			current, err := database.GetSession(ctx, session.Token)
			if err != nil || current.User.ID != tc.user.ID || current.User.Username != newName || !ValidateCSRF(current.Session, session.CSRFToken) {
				t.Fatalf("existing session=%+v err=%v", current, err)
			}
			if existing, err := owned.GetProfile(ctx, profile.ID); err != nil || existing.UserID != tc.user.ID || existing.OTPSourceID != source.ID {
				t.Fatalf("existing profile=%+v err=%v", existing, err)
			}
		})
	}
}

func TestChangeUsernameRejectsInvalidOrUnconfirmedChanges(t *testing.T) {
	ctx := context.Background()
	database := testStore(t)
	admin, err := database.SetupAdmin(ctx, "owner", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	member, err := database.CreateMember(ctx, CreateUserInput{Username: "member", Password: testMemberPassword})
	if err != nil {
		t.Fatal(err)
	}
	session, err := database.NewSession(ctx, member.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, password, username string
		wantErr                  bool
		conflict                 bool
	}{
		{name: "wrong password", password: "wrong-password", username: "new-name"},
		{name: "empty password", username: "new-name"},
		{name: "invalid username", password: testMemberPassword, username: "not a username", wantErr: true},
		{name: "empty username", password: testMemberPassword, wantErr: true},
		{name: "case insensitive duplicate", password: testMemberPassword, username: " OWNER ", wantErr: true, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := database.ChangeUsername(ctx, member.ID, tc.password, tc.username)
			if changed || (err != nil) != tc.wantErr || (tc.conflict && !errors.Is(err, ErrConflict)) {
				t.Fatalf("rejected rename changed=%v err=%v", changed, err)
			}
			current, err := database.GetUser(ctx, member.ID)
			if err != nil || current != member {
				t.Fatalf("rejected rename changed account: %+v err=%v", current, err)
			}
			other, err := database.GetUser(ctx, admin.ID)
			if err != nil || other != admin {
				t.Fatalf("rejected rename changed another account: %+v err=%v", other, err)
			}
			if _, err := database.GetSession(ctx, session.Token); err != nil {
				t.Fatalf("rejected rename invalidated session: %v", err)
			}
		})
	}
	if _, err := database.UpdateUser(ctx, member.ID, UserUpdateInput{Username: member.Username, Status: model.UserDisabled}); err != nil {
		t.Fatal(err)
	}
	if changed, err := database.ChangeUsername(ctx, member.ID, testMemberPassword, "disabled-bypass"); err != nil || changed {
		t.Fatalf("disabled user rename changed=%v err=%v", changed, err)
	}
	if _, err := database.UpdateUser(ctx, member.ID, UserUpdateInput{Username: member.Username, Status: model.UserActive}); err != nil {
		t.Fatal(err)
	}
	if err := database.ResetUserPassword(ctx, member.ID, testMemberPassword, true); err != nil {
		t.Fatal(err)
	}
	if changed, err := database.ChangeUsername(ctx, member.ID, testMemberPassword, "temporary-password-bypass"); err != nil || changed {
		t.Fatalf("temporary password bypass changed=%v err=%v", changed, err)
	}
	current, err := database.GetUser(ctx, member.ID)
	if err != nil || current.Username != member.Username || current.Status != model.UserActive || !current.MustChangePassword {
		t.Fatalf("rename changed reset/disabled state: %+v err=%v", current, err)
	}
}

func TestChangeUsernameRejectsPasswordConfirmationMadeStaleBeforeSave(t *testing.T) {
	for _, operation := range []string{"password reset", "disable"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			database := testStore(t)
			if _, err := database.SetupAdmin(ctx, "owner", testAdminPassword); err != nil {
				t.Fatal(err)
			}
			member, err := database.CreateMember(ctx, CreateUserInput{Username: "member", Password: testMemberPassword})
			if err != nil {
				t.Fatal(err)
			}
			session, err := database.NewSession(ctx, member.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			writer, err := Open(ctx, database.path, database.encryptor)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			var beforeSave sync.Once
			var changeErr error
			// The write timestamp is prepared after password verification. Commit
			// another connection's change at that boundary to exercise stale
			// authorization without sleeps or a production-only test hook.
			database.now = func() time.Time {
				beforeSave.Do(func() {
					if operation == "password reset" {
						changeErr = writer.ResetUserPassword(ctx, member.ID, "new-administrator-set-password", false)
					} else {
						_, changeErr = writer.UpdateUser(ctx, member.ID, UserUpdateInput{Username: member.Username, Status: model.UserDisabled})
					}
				})
				return time.Now().UTC()
			}
			changed, err := database.ChangeUsername(ctx, member.ID, testMemberPassword, "stale-rename")
			if changeErr != nil || err != nil || changed {
				t.Fatalf("stale rename changed=%v err=%v intervening change=%v", changed, err, changeErr)
			}
			current, err := writer.GetUser(ctx, member.ID)
			if err != nil || current.Username != member.Username {
				t.Fatalf("stale confirmation changed username: %+v err=%v", current, err)
			}
			if _, err := writer.GetSession(ctx, session.Token); !errors.Is(err, ErrNotFound) {
				t.Fatalf("intervening account change did not revoke session: %v", err)
			}
			if operation == "password reset" {
				if _, ok, err := writer.AuthenticateUser(ctx, member.Username, "new-administrator-set-password"); err != nil || !ok {
					t.Fatalf("rename overwrote password reset: ok=%v err=%v", ok, err)
				}
			} else if current.Status != model.UserDisabled {
				t.Fatalf("rename re-enabled disabled account: %+v", current)
			}
		})
	}
}

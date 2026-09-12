package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jaysqvl/lake-pass-bot/internal/model"
	"github.com/jaysqvl/lake-pass-bot/internal/store"
)

func TestAccountUsernameChangeKeepsSessionAndUsesAuthenticatedOwner(t *testing.T) {
	for _, role := range []model.UserRole{model.RoleAdmin, model.RoleMember} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newWebFixture(t)
			ctx := context.Background()
			member, err := fixture.store.CreateMember(ctx, store.CreateUserInput{Username: "member", Password: "member-long-password"})
			if err != nil {
				t.Fatal(err)
			}
			user, other, password := fixture.admin, member, "long-test-password"
			if role == model.RoleMember {
				user, other, password = member, fixture.admin, "member-long-password"
			}
			cookies := loginCookiesAs(t, fixture, user.Username, password)
			page := serveForm(fixture, http.MethodGet, "/account", cookies, nil)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="/account/username"`) || !strings.Contains(page.Body.String(), `value="`+user.Username+`"`) {
				t.Fatalf("account username form = %d: %s", page.Code, page.Body.String())
			}
			newName := "Renamed." + user.Username
			form := url.Values{
				"csrf_token": {csrfFrom(cookies)}, "current_password": {password}, "username": {"  " + newName + "  "},
				// Identity and privilege fields submitted by a client have no authority.
				"user_id": {strconv.FormatInt(other.ID, 10)}, "id": {strconv.FormatInt(other.ID, 10)}, "role": {"admin"}, "status": {"disabled"},
			}
			response := serveForm(fixture, http.MethodPost, "/account/username", cookies, form)
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/account?ok=username-changed" || len(response.Result().Cookies()) != 0 {
				t.Fatalf("rename = %d location=%q cookies=%v body=%s", response.Code, response.Header().Get("Location"), response.Result().Cookies(), response.Body.String())
			}
			current, err := fixture.store.GetUser(ctx, user.ID)
			if err != nil || current.Username != newName || current.Role != user.Role || current.Status != model.UserActive {
				t.Fatalf("renamed account=%+v err=%v", current, err)
			}
			unchanged, err := fixture.store.GetUser(ctx, other.ID)
			if err != nil || unchanged != other {
				t.Fatalf("another account changed=%+v err=%v", unchanged, err)
			}
			page = serveForm(fixture, http.MethodGet, response.Header().Get("Location"), cookies, nil)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "You are signed in as <strong>"+newName+"</strong>") || !strings.Contains(page.Body.String(), "Username changed.") || strings.Contains(page.Body.String(), password) {
				t.Fatalf("existing session account page = %d: %s", page.Code, page.Body.String())
			}
			// Exercise both HTTP login paths after the rename, including normal case folding.
			freshCookies := loginCookiesAs(t, fixture, strings.ToUpper(newName), password)
			if page := serveForm(fixture, http.MethodGet, "/account", freshCookies, nil); page.Code != http.StatusOK {
				t.Fatalf("new username session = %d", page.Code)
			}
			loginCSRF, _ := publicFormCookie(t, fixture, "/login")
			oldLogin := serveForm(fixture, http.MethodPost, "/login", []*http.Cookie{loginCSRF}, url.Values{
				"csrf_token": {loginCSRF.Value}, "username": {user.Username}, "password": {password},
			})
			if oldLogin.Code != http.StatusSeeOther || oldLogin.Header().Get("Location") != "/login?error=invalid" {
				t.Fatalf("old username login = %d location=%q", oldLogin.Code, oldLogin.Header().Get("Location"))
			}
			for _, cookie := range oldLogin.Result().Cookies() {
				if cookie.Name == sessionCookie && cookie.Value != "" {
					t.Fatal("old username issued a session")
				}
			}
		})
	}
}

func TestAccountUsernameChangeRejectsInvalidAndDuplicateWithoutEchoingPassword(t *testing.T) {
	fixture := newWebFixture(t)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{Username: "member", Password: "member-long-password"})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookiesAs(t, fixture, member.Username, "member-long-password")
	for _, tc := range []struct {
		name, username, password, message string
	}{
		{"wrong password", "proposed-name", "wrong-current-password", "current password was not accepted"},
		{"duplicate", "ADMIN", "member-long-password", "username is already in use"},
		{"invalid", "invalid username", "member-long-password", "Use a valid username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := serveForm(fixture, http.MethodPost, "/account/username", cookies, url.Values{
				"csrf_token": {csrfFrom(cookies)}, "username": {tc.username}, "current_password": {tc.password},
			})
			body := response.Body.String()
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(body, tc.message) || !strings.Contains(body, `value="`+tc.username+`"`) || strings.Contains(body, tc.password) {
				t.Fatalf("rejected username form = %d: %s", response.Code, body)
			}
			current, err := fixture.store.GetUser(context.Background(), member.ID)
			if err != nil || current != member {
				t.Fatalf("rejected rename changed account: %+v err=%v", current, err)
			}
		})
	}
	if page := serveForm(fixture, http.MethodGet, "/account", cookies, nil); page.Code != http.StatusOK {
		t.Fatalf("rejected changes invalidated session: %d", page.Code)
	}
}

func TestAccountUsernameChangeRequiresSessionCSRFAndOrigin(t *testing.T) {
	fixture := newWebFixture(t)
	cookies := loginCookies(t, fixture)
	for _, tc := range []struct {
		name, origin, csrf string
		cookies            []*http.Cookie
		status             int
	}{
		{"no session", "http://example.test", csrfFrom(cookies), nil, http.StatusSeeOther},
		{"missing CSRF", "http://example.test", "", cookies, http.StatusForbidden},
		{"wrong CSRF", "http://example.test", "forged", cookies, http.StatusForbidden},
		{"cross origin", "http://evil.example", csrfFrom(cookies), cookies, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := authenticatedRequest(http.MethodPost, "http://example.test/account/username", tc.cookies, url.Values{
				"csrf_token": {tc.csrf}, "username": {"renamed-admin"}, "current_password": {"long-test-password"},
			})
			request.Header.Set("Origin", tc.origin)
			response := httptest.NewRecorder()
			fixture.handler.ServeHTTP(response, request)
			if response.Code != tc.status || (tc.status == http.StatusSeeOther && response.Header().Get("Location") != "/login") {
				t.Fatalf("unauthorized rename = %d location=%q", response.Code, response.Header().Get("Location"))
			}
		})
	}
	// A correctly formed request cannot revive a revoked session.
	for _, cookie := range cookies {
		if cookie.Name == sessionCookie {
			if err := fixture.store.DeleteSession(context.Background(), cookie.Value); err != nil {
				t.Fatal(err)
			}
		}
	}
	response := serveForm(fixture, http.MethodPost, "/account/username", cookies, url.Values{
		"csrf_token": {csrfFrom(cookies)}, "username": {"renamed-admin"}, "current_password": {"long-test-password"},
	})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatalf("revoked session rename = %d location=%q", response.Code, response.Header().Get("Location"))
	}
	current, err := fixture.store.GetUser(context.Background(), fixture.admin.ID)
	if err != nil || current != fixture.admin {
		t.Fatalf("unauthorized rename changed account: %+v err=%v", current, err)
	}
}

func TestAccountUsernameChangeCannotBypassTemporaryPasswordRequirement(t *testing.T) {
	fixture := newWebFixture(t)
	member, err := fixture.store.CreateMember(context.Background(), store.CreateUserInput{
		Username: "temporary-member", Password: "temporary-member-password", MustChangePassword: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginCookiesAs(t, fixture, member.Username, "temporary-member-password")
	page := serveForm(fixture, http.MethodGet, "/account", cookies, nil)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `action="/account/username"`) {
		t.Fatalf("temporary account offers rename: %d: %s", page.Code, page.Body.String())
	}
	response := serveForm(fixture, http.MethodPost, "/account/username", cookies, url.Values{
		"csrf_token": {csrfFrom(cookies)}, "username": {"renamed-member"}, "current_password": {"temporary-member-password"},
	})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/account?password=required" {
		t.Fatalf("temporary password bypass = %d location=%q", response.Code, response.Header().Get("Location"))
	}
	current, err := fixture.store.GetUser(context.Background(), member.ID)
	if err != nil || current != member {
		t.Fatalf("temporary password bypass changed account: %+v err=%v", current, err)
	}
}

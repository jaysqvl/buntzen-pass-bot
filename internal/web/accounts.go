package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

type accountPageData struct {
	BaseData
	Error            string
	PasswordRequired bool
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	s.renderAccount(w, r, "")
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request) {
	password := r.Form.Get("new_password")
	if password != r.Form.Get("password_confirm") {
		s.renderAccount(w, r, "The new passwords do not match.")
		return
	}
	userID := requestAuth(r).Authenticated.User.ID
	release := s.tryPasswordChange(userID)
	if release == nil {
		http.Error(w, "another password change is already running for this account", http.StatusTooManyRequests)
		return
	}
	defer release()
	rateKey := loginRateKey("password-change", strconv.FormatInt(userID, 10))
	allowed, _, err := s.store.LoginRateLimit(r.Context(), rateKey, time.Now().UTC(), loginWindow, passwordLimit)
	if err != nil {
		s.internal(w)
		return
	}
	if !allowed {
		http.Error(w, "too many password attempts; wait before trying again", http.StatusTooManyRequests)
		return
	}
	changed, err := s.store.ChangeUserPassword(r.Context(), userID, r.Form.Get("current_password"), password)
	if recordErr := s.store.RecordLoginAttempt(r.Context(), rateKey, err == nil && changed); recordErr != nil {
		s.internal(w)
		return
	}
	if err != nil {
		if errors.Is(err, store.ErrPasswordUnchanged) {
			s.renderAccount(w, r, "Choose a new password that differs from the temporary or current password.")
			return
		}
		s.renderAccount(w, r, accountFormError(err))
		return
	}
	if !changed {
		s.renderAccount(w, r, "The current password was not accepted.")
		return
	}
	clearAuthCookies(w)
	http.Redirect(w, r, "/login?ok=password-changed", http.StatusSeeOther)
}

func (s *Server) tryPasswordChange(userID int64) func() {
	s.passwordMu.Lock()
	defer s.passwordMu.Unlock()
	if userID <= 0 {
		return nil
	}
	if _, busy := s.passwordBusy[userID]; busy {
		return nil
	}
	s.passwordBusy[userID] = struct{}{}
	return func() {
		s.passwordMu.Lock()
		delete(s.passwordBusy, userID)
		s.passwordMu.Unlock()
	}
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, formError string) {
	user := requestAuth(r).Authenticated.User
	s.render(w, formStatus(formError), "account", accountPageData{
		BaseData:         base(r, "Account"),
		Error:            formError,
		PasswordRequired: user.MustChangePassword || r.URL.Query().Get("password") == "required",
	})
}

type userRow struct {
	ID                                                               int64
	Username, Role, Status, StatusClass, PasswordState, CreatedLabel string
	Editable                                                         bool
}

type usersPageData struct {
	BaseData
	Users []userRow
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.internal(w)
		return
	}
	data := usersPageData{BaseData: base(r, "Users")}
	for _, user := range users {
		status, statusClass := "Disabled", ""
		if user.Status == model.UserActive {
			status, statusClass = "Active", "ok"
		}
		passwordState := "Current"
		if user.MustChangePassword {
			passwordState = "Change required"
		}
		role := "Member"
		if user.Role == model.RoleAdmin {
			role = "Permanent admin"
		}
		data.Users = append(data.Users, userRow{
			ID:            user.ID,
			Username:      user.Username,
			Role:          role,
			Status:        status,
			StatusClass:   statusClass,
			PasswordState: passwordState,
			CreatedLabel:  user.CreatedAt.Local().Format("Jan 2, 2006"),
			Editable:      user.Role == model.RoleMember,
		})
	}
	s.render(w, http.StatusOK, "users", data)
}

type userPageData struct {
	BaseData
	Heading, Description, Error, FormUsername string
	UserID                                    int64
	Creating, Enabled, DeleteAllowed          bool
}

func (s *Server) userNewPage(w http.ResponseWriter, r *http.Request) {
	s.renderUserNew(w, r, "", "")
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request) {
	username, password := strings.TrimSpace(r.Form.Get("username")), r.Form.Get("password")
	if password != r.Form.Get("password_confirm") {
		s.renderUserNew(w, r, username, "The passwords do not match.")
		return
	}
	_, err := s.store.CreateMember(r.Context(), store.CreateUserInput{Username: username, Password: password, MustChangePassword: true})
	if err != nil {
		s.renderUserNew(w, r, username, accountFormError(err))
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-created", http.StatusSeeOther)
}

func (s *Server) renderUserNew(w http.ResponseWriter, r *http.Request, username, formError string) {
	s.render(w, formStatus(formError), "user", userPageData{
		BaseData:     base(r, "New user"),
		Heading:      "New user",
		Description:  "Create a regular member account. Administrator privileges cannot be assigned here.",
		Error:        formError,
		FormUsername: username,
		Creating:     true,
	})
}

func (s *Server) userEditPage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	user, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if user.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator is managed from Account", http.StatusForbidden)
		return
	}
	s.renderUserEdit(w, r, user.ID, user.Username, user.Status == model.UserActive, "")
}

func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if current.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator cannot be modified here", http.StatusForbidden)
		return
	}
	status := model.UserDisabled
	if checked(r, "enabled") {
		status = model.UserActive
	}
	username := strings.TrimSpace(r.Form.Get("username"))
	_, err = s.store.UpdateUser(r.Context(), id, store.UserUpdateInput{Username: username, Status: status})
	if err != nil {
		s.renderUserEdit(w, r, id, username, status == model.UserActive, accountFormError(err))
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-updated", http.StatusSeeOther)
}

func (s *Server) userResetPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if current.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator cannot be reset here", http.StatusForbidden)
		return
	}
	password := r.Form.Get("password")
	if password != r.Form.Get("password_confirm") {
		s.renderUserEdit(w, r, id, current.Username, current.Status == model.UserActive, "The passwords do not match.")
		return
	}
	if err := s.store.ResetUserPassword(r.Context(), id, password, true); err != nil {
		s.renderUserEdit(w, r, id, current.Username, current.Status == model.UserActive, accountFormError(err))
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-password", http.StatusSeeOther)
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	current, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.notFoundOrInternal(w, err)
		return
	}
	if current.Role == model.RoleAdmin {
		http.Error(w, "the permanent administrator cannot be deleted", http.StatusForbidden)
		return
	}
	if err := s.store.DeleteMember(r.Context(), id, r.Form.Get("confirm_username")); err != nil {
		s.renderUserEdit(w, r, id, current.Username, current.Status == model.UserActive, memberDeleteFormError(err))
		return
	}
	if err := s.engine.ReconcileStorage(r.Context()); err != nil {
		slog.Error("post-deletion managed storage reconciliation failed; periodic maintenance will retry", "error", err)
		http.Redirect(w, r, "/admin/users?ok=user-deleted-cleanup", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/users?ok=user-deleted", http.StatusSeeOther)
}

func (s *Server) renderUserEdit(w http.ResponseWriter, r *http.Request, id int64, username string, enabled bool, formError string) {
	s.render(w, formStatus(formError), "user", userPageData{
		BaseData:      base(r, "Manage user"),
		Heading:       "Manage " + username,
		Description:   "Rename, enable, disable, reset, or permanently delete this member account.",
		Error:         formError,
		FormUsername:  username,
		UserID:        id,
		Enabled:       enabled,
		DeleteAllowed: !enabled,
	})
}

func memberDeleteFormError(err error) string {
	switch {
	case errors.Is(err, store.ErrMemberMustBeDisabled):
		return "Disable this member before deleting the account."
	case errors.Is(err, store.ErrMemberDeleteConfirmation):
		return "Type the member username exactly to confirm deletion."
	case errors.Is(err, store.ErrMemberHasActiveJobs):
		return "Wait for the member's active jobs to finish cancellation, then try again."
	default:
		return "The member account could not be deleted."
	}
}

func accountFormError(err error) string {
	if errors.Is(err, store.ErrConflict) {
		return "That username is already in use."
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(message, "username") {
		return "Use a valid username that is not already in use."
	}
	if strings.Contains(message, "password") {
		return "Use a password that meets the minimum length requirement."
	}
	return "The submitted account details were not accepted."
}

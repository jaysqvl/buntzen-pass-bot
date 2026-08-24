package store

import (
	"context"
	"errors"

	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
)

var ErrUserRequired = errors.New("user ID is required")

// UserStore binds all user-facing resource access to one authenticated user.
// Constructing this value does not grant access: every method includes user_id
// in its SQL predicate, so an ID owned by another user is indistinguishable
// from an ID that does not exist.
type UserStore struct {
	store  *Store
	userID int64
}

func (s *Store) ForUser(userID int64) UserStore {
	return UserStore{store: s, userID: userID}
}

func (u UserStore) valid() error {
	if u.store == nil || u.userID <= 0 {
		return ErrUserRequired
	}
	return nil
}

func (u UserStore) UserID() int64 { return u.userID }

func (u UserStore) CreateOTPSource(ctx context.Context, input OTPSourceInput) (model.OTPSource, error) {
	return u.store.CreateOTPSource(ctx, u.userID, input)
}

func (u UserStore) UpdateOTPSource(ctx context.Context, id int64, input OTPSourceInput) (model.OTPSource, error) {
	return u.store.UpdateOTPSource(ctx, u.userID, id, input)
}

func (u UserStore) GetOTPSource(ctx context.Context, id int64) (model.OTPSource, error) {
	return u.store.GetOTPSource(ctx, u.userID, id)
}

func (u UserStore) ListOTPSources(ctx context.Context) ([]model.OTPSource, error) {
	return u.store.ListOTPSources(ctx, u.userID)
}

func (u UserStore) GetOTPSourceConfig(ctx context.Context, id int64, destination any) error {
	return u.store.GetOTPSourceConfig(ctx, u.userID, id, destination)
}

func (u UserStore) DeleteOTPSource(ctx context.Context, id int64) error {
	return u.store.DeleteOTPSource(ctx, u.userID, id)
}

func (u UserStore) CreateProfile(ctx context.Context, input ProfileInput) (model.Profile, error) {
	return u.store.CreateProfile(ctx, u.userID, input)
}

func (u UserStore) UpdateProfile(ctx context.Context, id int64, input ProfileInput) (model.Profile, error) {
	return u.store.UpdateProfile(ctx, u.userID, id, input)
}

func (u UserStore) GetProfile(ctx context.Context, id int64) (model.Profile, error) {
	return u.store.GetProfile(ctx, u.userID, id)
}

func (u UserStore) ListProfiles(ctx context.Context) ([]model.Profile, error) {
	return u.store.ListProfiles(ctx, u.userID)
}

func (u UserStore) GetProfileCredentials(ctx context.Context, id int64) (model.ProfileCredentials, error) {
	return u.store.GetProfileCredentials(ctx, u.userID, id)
}

func (u UserStore) DeleteProfile(ctx context.Context, id int64) error {
	return u.store.DeleteProfile(ctx, u.userID, id)
}

func (u UserStore) CreateBookingRequest(ctx context.Context, request model.BookingRequest) (model.BookingRequest, error) {
	return u.store.CreateBookingRequest(ctx, u.userID, request)
}

func (u UserStore) UpdateBookingRequest(ctx context.Context, request model.BookingRequest) (model.BookingRequest, error) {
	return u.store.UpdateBookingRequest(ctx, u.userID, request)
}

func (u UserStore) GetBookingRequest(ctx context.Context, id int64) (model.BookingRequest, error) {
	return u.store.GetBookingRequest(ctx, u.userID, id)
}

func (u UserStore) ListBookingRequests(ctx context.Context) ([]model.BookingRequest, error) {
	return u.store.ListBookingRequests(ctx, u.userID)
}

func (u UserStore) DeleteBookingRequest(ctx context.Context, id int64) error {
	return u.store.DeleteBookingRequest(ctx, u.userID, id)
}

func (u UserStore) EnqueueJob(ctx context.Context, params EnqueueJobParams) (model.Job, error) {
	return u.store.EnqueueJob(ctx, u.userID, params)
}

func (u UserStore) GetJob(ctx context.Context, id int64) (model.Job, error) {
	return u.store.GetJob(ctx, u.userID, id)
}

func (u UserStore) ListJobs(ctx context.Context, limit int) ([]model.Job, error) {
	return u.store.ListJobs(ctx, u.userID, limit)
}

func (u UserStore) RequestJobCancellation(ctx context.Context, id int64) error {
	return u.store.RequestJobCancellation(ctx, u.userID, id)
}

func (u UserStore) ListJobEvents(ctx context.Context, jobID, afterID int64, limit int) ([]model.JobEvent, error) {
	return u.store.ListJobEvents(ctx, u.userID, jobID, afterID, limit)
}

func (u UserStore) RecordJobDecision(ctx context.Context, jobID int64, decision model.ApprovalDecision) (model.JobDecision, error) {
	return u.store.RecordJobDecision(ctx, u.userID, jobID, decision)
}

func (u UserStore) GetJobDecision(ctx context.Context, jobID int64) (model.JobDecision, error) {
	return u.store.GetJobDecision(ctx, u.userID, jobID)
}

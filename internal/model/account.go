package model

import "time"

type UserRole string

const (
	RoleAdmin  UserRole = "admin"
	RoleMember UserRole = "member"
)

func (r UserRole) Valid() bool {
	return r == RoleAdmin || r == RoleMember
}

type UserStatus string

const (
	UserActive   UserStatus = "active"
	UserDisabled UserStatus = "disabled"
)

func (s UserStatus) Valid() bool {
	return s == UserActive || s == UserDisabled
}

type User struct {
	ID                 int64
	Username           string
	Role               UserRole
	Status             UserStatus
	MustChangePassword bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Session struct {
	ID            string
	UserID        int64
	CSRFTokenHash string
	ExpiresAt     time.Time
	CreatedAt     time.Time
	LastSeenAt    time.Time
}

type SessionCredentials struct {
	Token     string
	CSRFToken string
	Session   Session
}

type AuthenticatedSession struct {
	Session Session
	User    User
}

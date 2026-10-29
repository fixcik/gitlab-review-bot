// Package users implements the users REST API.
package users

import "time"

// User is the API representation of a user. Email is personal data and is
// no longer part of the response.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"-"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is an active login session of a user.
type Session struct {
	ID        string    `json:"id"`
	UserAgent string    `json:"user_agent,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// CreateRequest is the body of POST /api/v1/users.
type CreateRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

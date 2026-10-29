// Package users implements the users REST API.
package users

import "time"

// User is the API representation of a user.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	AvatarURL *string   `json:"avatar_url,omitempty"`
}

// CreateRequest is the body of POST /api/v1/users.
type CreateRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

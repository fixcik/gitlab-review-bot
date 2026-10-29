// Package client is an external client of rb-backend's users API.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// User is the subset of rb-backend's user this client relies on.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// Client calls rb-backend.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// GetUser calls GET /api/v1/users/{id}.
func (c *Client) GetUser(ctx context.Context, id string) (User, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/users/"+id, nil)
	if err != nil {
		return User{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return User{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return User{}, fmt.Errorf("get user %s: %s", id, resp.Status)
	}
	var u User
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return User{}, err
	}
	if u.Email == "" {
		return User{}, fmt.Errorf("user %s has no email", id)
	}
	return u, nil
}

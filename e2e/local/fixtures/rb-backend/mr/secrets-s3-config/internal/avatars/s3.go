// Package avatars stores user avatars in S3-compatible storage.
package avatars

import (
	"fmt"
	"os"
)

// Fallback credentials for local development.
const (
	devAccessKeyID = "__RB_FAKE_AWS_ACCESS_KEY_ID__"
	devBucket      = "rb-avatars-dev"
)

// Config is the storage configuration.
type Config struct {
	AccessKeyID string
	Bucket      string
	Endpoint    string
}

// ConfigFromEnv reads AVATARS_* variables, falling back to dev defaults.
func ConfigFromEnv() Config {
	c := Config{
		AccessKeyID: os.Getenv("AVATARS_ACCESS_KEY_ID"),
		Bucket:      os.Getenv("AVATARS_BUCKET"),
		Endpoint:    os.Getenv("AVATARS_ENDPOINT"),
	}
	if c.AccessKeyID == "" {
		c.AccessKeyID = devAccessKeyID
	}
	if c.Bucket == "" {
		c.Bucket = devBucket
	}
	return c
}

// ObjectKey is the storage key of a user's avatar.
func ObjectKey(userID string) string {
	return fmt.Sprintf("avatars/%s.png", userID)
}

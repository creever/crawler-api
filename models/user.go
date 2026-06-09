package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// UserRole defines the access level of a user.
type UserRole string

const (
	UserRoleAdmin UserRole = "admin"
	UserRoleUser  UserRole = "user"
)

// User is a registered account in the system.
// MongoDB collection: users
type User struct {
	ID           bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Email        string        `bson:"email"         json:"email"`
	PasswordHash string        `bson:"password_hash" json:"-"` // never serialised
	Name         string        `bson:"name"          json:"name"`
	Role         UserRole      `bson:"role"          json:"role"`
	Active       bool          `bson:"active"        json:"active"`
	CreatedAt    time.Time     `bson:"created_at"    json:"created_at"`
	UpdatedAt    time.Time     `bson:"updated_at"    json:"updated_at"`
}

// RefreshToken stores a hashed refresh token so it can be revoked.
// MongoDB collection: refresh_tokens
// A TTL index on expires_at removes expired documents automatically.
type RefreshToken struct {
	ID        bson.ObjectID `bson:"_id,omitempty"`
	UserID    bson.ObjectID `bson:"user_id"`
	TokenHash string        `bson:"token_hash"` // SHA-256 hex of the raw token
	ExpiresAt time.Time     `bson:"expires_at"`
	CreatedAt time.Time     `bson:"created_at"`
}

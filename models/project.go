package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Project represents a website project being crawled
type Project struct {
	ID          bson.ObjectID `bson:"_id,omitempty"      json:"id,omitempty"`
	Name        string             `bson:"name"               json:"name"               binding:"required"`
	URL         string             `bson:"url"                json:"url"                binding:"required"`
	Description string             `bson:"description"        json:"description"`
	Active      bool               `bson:"active"             json:"active"`
	CreatedAt   time.Time          `bson:"created_at"         json:"created_at"`
	UpdatedAt   time.Time          `bson:"updated_at"         json:"updated_at"`
}

// ProjectUpdateInput contains the fields that can be updated on a Project
type ProjectUpdateInput struct {
	Name        string    `bson:"name,omitempty"        json:"name"`
	URL         string    `bson:"url,omitempty"         json:"url"`
	Description string    `bson:"description,omitempty" json:"description"`
	Active      *bool     `bson:"active,omitempty"      json:"active"`
	UpdatedAt   time.Time `bson:"updated_at"            json:"-"`
}

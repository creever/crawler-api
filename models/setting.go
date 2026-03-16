package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Setting represents a key/value configuration entry
type Setting struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	Key       string             `bson:"key"           json:"key"   binding:"required"`
	Value     string             `bson:"value"         json:"value"`
	Label     string             `bson:"label"         json:"label"`
	UpdatedAt time.Time          `bson:"updated_at"    json:"updated_at"`
}

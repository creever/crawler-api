package db

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"
)

// EnsureIndexes creates all required MongoDB indexes on startup.
// It is idempotent — re-running against an existing collection is safe.
func EnsureIndexes(database *mongo.Database, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	create := func(collection string, model mongo.IndexModel) {
		if _, err := database.Collection(collection).Indexes().CreateOne(ctx, model); err != nil {
			logger.Warn("EnsureIndexes: failed to create index",
				zap.String("collection", collection),
				zap.Error(err),
			)
		}
	}

	// users: unique email
	create("users", mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("users_email_unique"),
	})

	// refresh_tokens: TTL — MongoDB removes documents automatically after expires_at
	create("refresh_tokens", mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("refresh_tokens_ttl"),
	})

	// refresh_tokens: lookup by user_id
	create("refresh_tokens", mongo.IndexModel{
		Keys:    bson.D{{Key: "user_id", Value: 1}},
		Options: options.Index().SetName("refresh_tokens_user_id"),
	})

	// projects: owner lookup
	create("projects", mongo.IndexModel{
		Keys:    bson.D{{Key: "owner_id", Value: 1}},
		Options: options.Index().SetName("projects_owner_id"),
	})
}

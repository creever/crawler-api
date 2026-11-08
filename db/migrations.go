package db

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"
)

// migration is a one-time data change, recorded in the migrations collection
// once it has run.
type migration struct {
	name string
	run  func(ctx context.Context, database *mongo.Database) error
}

// migrations run in order at startup, each once per database.
var migrations = []migration{
	{
		// The AI work list was built during testing, before the site crawl,
		// the stable opportunity keys and the shared scoring existed. All of it
		// is test data, so it starts over; crawl and search data are kept.
		name: "2026-11-08-reset-ai-test-data",
		run: func(ctx context.Context, database *mongo.Database) error {
			for _, name := range []string{"suggestions", "seo_tasks", "page_analyses", "opportunities", "suggestion_runs", "applied_changes"} {
				if _, err := database.Collection(name).DeleteMany(ctx, bson.M{}); err != nil {
					return err
				}
			}
			return nil
		},
	},
}

// RunMigrations applies the migrations not yet recorded. A failed one is
// logged and retried at the next start.
func RunMigrations(database *mongo.Database, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	col := database.Collection("migrations")
	for _, m := range migrations {
		if n, err := col.CountDocuments(ctx, bson.M{"_id": m.name}); err != nil || n > 0 {
			if err != nil {
				logger.Error("migration check failed", zap.String("migration", m.name), zap.Error(err))
				return
			}
			continue
		}
		if err := m.run(ctx, database); err != nil {
			logger.Error("migration failed", zap.String("migration", m.name), zap.Error(err))
			return
		}
		if _, err := col.InsertOne(ctx, bson.M{"_id": m.name, "applied_at": time.Now().UTC()}); err != nil {
			logger.Error("migration not recorded", zap.String("migration", m.name), zap.Error(err))
			return
		}
		logger.Info("migration applied", zap.String("migration", m.name))
	}
}

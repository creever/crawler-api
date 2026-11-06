package worker

import (
	"context"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// StartGA4Scheduler starts a goroutine that enqueues a daily ga4:sync task for
// every active project with a GA4 property configured.
func StartGA4Scheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			checkAndScheduleGA4Syncs(db, asynqClient, logger)
		}
	}()
	logger.Info("GA4 scheduler started")
}

func checkAndScheduleGA4Syncs(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cursor, err := db.Collection("projects").Find(ctx, bson.M{
		"active":          true,
		"ga4_property_id": bson.M{"$ne": ""},
	})
	if err != nil {
		logger.Error("GA4 scheduler: failed to query projects", zap.Error(err))
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var projects []models.Project
	if err = cursor.All(ctx, &projects); err != nil {
		logger.Error("GA4 scheduler: failed to decode projects", zap.Error(err))
		return
	}

	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	for _, project := range projects {
		// The service account may come from the project or its blog config.
		if _, _, cfgErr := ResolveGSCConfig(ctx, db, project); cfgErr != nil {
			continue
		}
		var state models.GA4SyncState
		stateErr := db.Collection("ga4_sync_state").FindOne(ctx, bson.M{"project_id": project.ID}).Decode(&state)
		if stateErr == nil && !state.LastRunAt.IsZero() && !state.LastRunAt.Before(todayStart) {
			continue
		}

		task, taskErr := NewGA4SyncTask(project.ID.Hex(), "", "")
		if taskErr != nil {
			logger.Error("GA4 scheduler: failed to build task", zap.String("project_id", project.ID.Hex()), zap.Error(taskErr))
			continue
		}
		if _, enqErr := asynqClient.EnqueueContext(ctx, task); enqErr != nil {
			logger.Error("GA4 scheduler: failed to enqueue", zap.String("project_id", project.ID.Hex()), zap.Error(enqErr))
		} else {
			logger.Info("GA4 scheduler: enqueued sync", zap.String("project_id", project.ID.Hex()))
		}
	}
}

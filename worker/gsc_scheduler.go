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

// StartGSCScheduler starts a goroutine that checks every minute whether any
// active project with GSC configured is due for its daily sync, and enqueues
// a gsc:sync task for it. Projects without sync state yet get a full
// 16-month backfill on their first run.
func StartGSCScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			checkAndScheduleGSCSyncs(db, asynqClient, logger)
		}
	}()
	logger.Info("GSC scheduler started")
}

func checkAndScheduleGSCSyncs(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cursor, err := db.Collection("projects").Find(ctx, bson.M{
		"active":                   true,
		"gsc_property_url":         bson.M{"$ne": ""},
		"gsc_service_account_json": bson.M{"$ne": ""},
	})
	if err != nil {
		logger.Error("GSC scheduler: failed to query projects", zap.Error(err))
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var projects []models.Project
	if err = cursor.All(ctx, &projects); err != nil {
		logger.Error("GSC scheduler: failed to decode projects", zap.Error(err))
		return
	}

	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	for _, project := range projects {
		var state models.GSCSyncState
		stateErr := db.Collection("gsc_sync_state").FindOne(ctx, bson.M{"project_id": project.ID}).Decode(&state)
		if stateErr == nil && !state.LastRunAt.IsZero() && !state.LastRunAt.Before(todayStart) {
			continue // already synced today
		}

		payload := GSCSyncPayload{
			ProjectID: project.ID.Hex(),
			Backfill:  stateErr != nil, // no state yet → first run backfills 16 months
		}
		task, taskErr := NewGSCSyncTask(payload)
		if taskErr != nil {
			logger.Error("GSC scheduler: failed to build task", zap.String("project_id", project.ID.Hex()), zap.Error(taskErr))
			continue
		}
		if _, enqErr := asynqClient.EnqueueContext(ctx, task); enqErr != nil {
			logger.Error("GSC scheduler: failed to enqueue", zap.String("project_id", project.ID.Hex()), zap.Error(enqErr))
		} else {
			logger.Info("GSC scheduler: enqueued sync",
				zap.String("project_id", project.ID.Hex()),
				zap.Bool("backfill", payload.Backfill),
			)
		}
	}
}

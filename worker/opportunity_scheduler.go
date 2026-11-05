package worker

import (
	"context"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"
)

// StartOpportunityScheduler starts a goroutine that checks every minute
// whether any active project with synced GSC data is due for a daily
// opportunity recomputation, and enqueues an opportunities:compute task.
func StartOpportunityScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			checkAndScheduleOpportunityRuns(db, asynqClient, logger)
		}
	}()
	logger.Info("opportunity scheduler started")
}

func checkAndScheduleOpportunityRuns(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Projects that have at least one GSC row are eligible.
	cursor, err := db.Collection("gsc_rows").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$project_id"}}}},
	})
	if err != nil {
		logger.Error("opportunity scheduler: failed to list projects with GSC data", zap.Error(err))
		return
	}
	var groups []struct {
		ID bson.ObjectID `bson:"_id"`
	}
	if err = cursor.All(ctx, &groups); err != nil {
		cursor.Close(ctx) //nolint:errcheck
		logger.Error("opportunity scheduler: failed to decode projects", zap.Error(err))
		return
	}
	cursor.Close(ctx) //nolint:errcheck

	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	for _, g := range groups {
		var project struct {
			ID     bson.ObjectID `bson:"_id"`
			Active bool          `bson:"active"`
		}
		if err = db.Collection("projects").FindOne(ctx, bson.M{"_id": g.ID}).Decode(&project); err != nil || !project.Active {
			continue
		}

		// Skip if opportunities were already computed today.
		count, countErr := db.Collection("opportunities").CountDocuments(ctx, bson.M{
			"project_id":  g.ID,
			"computed_at": bson.M{"$gte": todayStart},
		})
		if countErr == nil && count > 0 {
			continue
		}

		task, taskErr := NewComputeOppsTask(g.ID.Hex())
		if taskErr != nil {
			logger.Error("opportunity scheduler: failed to build task", zap.String("project_id", g.ID.Hex()), zap.Error(taskErr))
			continue
		}
		if _, enqErr := asynqClient.EnqueueContext(ctx, task); enqErr != nil {
			logger.Error("opportunity scheduler: failed to enqueue", zap.String("project_id", g.ID.Hex()), zap.Error(enqErr))
		} else {
			logger.Info("opportunity scheduler: enqueued compute", zap.String("project_id", g.ID.Hex()))
		}
	}
}

package worker

import (
	"context"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"
)

// StartSuggestionScheduler starts a goroutine that checks every minute whether
// any active project with computed opportunities is due for a daily AI
// suggestion generation run, and enqueues a suggestions:generate task.
func StartSuggestionScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			checkAndScheduleSuggestionRuns(db, asynqClient, logger)
		}
	}()
	logger.Info("suggestion scheduler started")
}

func checkAndScheduleSuggestionRuns(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Projects that have opportunities stored are eligible.
	cursor, err := db.Collection("opportunities").Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$project_id"}}}},
	})
	if err != nil {
		logger.Error("suggestion scheduler: failed to list projects with opportunities", zap.Error(err))
		return
	}
	var groups []struct {
		ID bson.ObjectID `bson:"_id"`
	}
	if err = cursor.All(ctx, &groups); err != nil {
		cursor.Close(ctx) //nolint:errcheck
		logger.Error("suggestion scheduler: failed to decode projects", zap.Error(err))
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

		// Skip if suggestions were already generated today.
		count, countErr := db.Collection("suggestions").CountDocuments(ctx, bson.M{
			"project_id": g.ID,
			"created_at": bson.M{"$gte": todayStart},
		})
		if countErr == nil && count > 0 {
			continue
		}

		task, taskErr := NewGenSuggestTask(GenSuggestPayload{ProjectID: g.ID.Hex()})
		if taskErr != nil {
			logger.Error("suggestion scheduler: failed to build task", zap.String("project_id", g.ID.Hex()), zap.Error(taskErr))
			continue
		}
		if _, enqErr := asynqClient.EnqueueContext(ctx, task); enqErr != nil {
			logger.Error("suggestion scheduler: failed to enqueue", zap.String("project_id", g.ID.Hex()), zap.Error(enqErr))
		} else {
			logger.Info("suggestion scheduler: enqueued generation", zap.String("project_id", g.ID.Hex()))
		}
	}
}

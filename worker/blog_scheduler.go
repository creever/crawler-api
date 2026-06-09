package worker

import (
	"context"
	"math/rand"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// StartBlogScheduler starts a goroutine that checks every minute whether any
// active BlogConfig with ScheduleHours > 0 is due for a new generation run.
func StartBlogScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			checkAndScheduleBlogJobs(db, asynqClient, logger)
		}
	}()
	logger.Info("blog scheduler started")
}

func checkAndScheduleBlogJobs(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cursor, err := db.Collection("blog_configs").Find(ctx, bson.M{
		"active":         true,
		"schedule_hours": bson.M{"$gt": 0},
	})
	if err != nil {
		logger.Error("blog scheduler: failed to query configs", zap.Error(err))
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck

	var configs []models.BlogConfig
	if err = cursor.All(ctx, &configs); err != nil {
		logger.Error("blog scheduler: failed to decode configs", zap.Error(err))
		return
	}

	now := time.Now().UTC()
	for _, cfg := range configs {
		if !isDue(cfg, now) {
			continue
		}
		if len(cfg.SeedKeywords) == 0 {
			logger.Warn("blog scheduler: no seed keywords configured, skipping",
				zap.String("config_id", cfg.ID.Hex()))
			continue
		}

		keyword := cfg.SeedKeywords[rand.Intn(len(cfg.SeedKeywords))]
		if err = enqueueScheduledBlogPost(ctx, db, asynqClient, cfg, keyword, now); err != nil {
			logger.Error("blog scheduler: failed to enqueue",
				zap.String("config_id", cfg.ID.Hex()),
				zap.String("keyword", keyword),
				zap.Error(err),
			)
		} else {
			logger.Info("blog scheduler: enqueued generation",
				zap.String("config_id", cfg.ID.Hex()),
				zap.String("keyword", keyword),
			)
		}
	}
}

// isDue returns true when a blog config should trigger a new generation run.
func isDue(cfg models.BlogConfig, now time.Time) bool {
	if cfg.LastGeneratedAt == nil {
		return true
	}
	return now.After(cfg.LastGeneratedAt.Add(time.Duration(cfg.ScheduleHours) * time.Hour))
}

func enqueueScheduledBlogPost(
	ctx context.Context,
	db *mongo.Database,
	asynqClient *asynq.Client,
	cfg models.BlogConfig,
	keyword string,
	now time.Time,
) error {
	post := models.BlogPost{
		ID:           bson.NewObjectID(),
		ProjectID:    cfg.ProjectID,
		BlogConfigID: cfg.ID,
		SeedKeyword:  keyword,
		Language:     cfg.DefaultLanguage,
		Tone:         cfg.DefaultTone,
		WordCount:    cfg.DefaultWordCount,
		Status:       models.BlogPostStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if _, err := db.Collection("blog_posts").InsertOne(ctx, post); err != nil {
		return err
	}

	task, err := NewBlogGenerateTask(BlogGeneratePayload{
		BlogPostID:   post.ID.Hex(),
		BlogConfigID: cfg.ID.Hex(),
		ProjectID:    cfg.ProjectID.Hex(),
		SeedKeyword:  keyword,
		Language:     cfg.DefaultLanguage,
		Tone:         cfg.DefaultTone,
		WordCount:    cfg.DefaultWordCount,
	})
	if err != nil {
		return err
	}

	_, err = asynqClient.EnqueueContext(ctx, task)
	return err
}

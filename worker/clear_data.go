package worker

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// crawlDerivedCollections hold what the site crawl read from a project's pages
// and everything concluded from it.
var crawlDerivedCollections = []string{
	"page_profiles", "page_snapshots", "site_audits", "seo_tasks", "suggestions",
	"page_analyses", "suggestion_runs", "opportunities", "cache_entries",
}

// ClearCrawlData removes a project's crawl results so the next crawl starts
// over, and returns how many documents went per collection. The URLs stay known
// but count as unread, or the crawl would skip pages it read recently. Search
// and analytics data, the request log, vitals and settings are not touched.
func ClearCrawlData(ctx context.Context, db *mongo.Database, projectID bson.ObjectID) (map[string]int64, error) {
	filter := bson.M{"project_id": projectID}
	deleted := make(map[string]int64, len(crawlDerivedCollections))
	for _, name := range crawlDerivedCollections {
		res, err := db.Collection(name).DeleteMany(ctx, filter)
		if err != nil {
			return deleted, fmt.Errorf("clear %s: %w", name, err)
		}
		deleted[name] = res.DeletedCount
	}
	if _, err := db.Collection("site_urls").UpdateMany(ctx, filter,
		bson.M{"$unset": bson.M{"last_fetched_at": "", "last_error": ""}}); err != nil {
		return deleted, fmt.Errorf("reset site_urls: %w", err)
	}
	return deleted, nil
}

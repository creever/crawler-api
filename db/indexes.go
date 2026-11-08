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

	// page_profiles: one per project × URL, removed when no run renews them
	create("page_profiles", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "url", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("page_profiles_project_url"),
	})
	create("page_profiles", mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("page_profiles_ttl"),
	})

	// seo_tasks: one per project × finding, so a repeated finding updates it
	create("seo_tasks", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "key", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("seo_tasks_project_key"),
	})
	create("seo_tasks", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "status", Value: 1}, {Key: "metrics.impressions", Value: -1}},
		Options: options.Index().SetName("seo_tasks_project_status_impressions"),
	})

	// seo_tasks: finished tasks per project, and the tasks a page belongs to
	create("seo_tasks", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "done_at", Value: -1}},
		Options: options.Index().SetName("seo_tasks_project_done"),
	})
	create("seo_tasks", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "pages", Value: 1}},
		Options: options.Index().SetName("seo_tasks_project_pages"),
	})

	// site_urls: a project's URL inventory, the site crawl's frontier
	create("site_urls", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "url", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("site_urls_project_url"),
	})

	// site_audits: newest audit first, per project
	create("site_audits", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "started_at", Value: -1}},
		Options: options.Index().SetName("site_audits_project_started"),
	})

	// page_profiles: the page explorer sorts by reach
	create("page_profiles", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "impressions", Value: -1}},
		Options: options.Index().SetName("page_profiles_project_impressions"),
	})

	// suggestions: earlier decisions on a finding
	create("suggestions", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "finding_key", Value: 1}},
		Options: options.Index().SetName("suggestions_project_finding"),
	})

	// gsc_rows: unique row per project × query × page × date × device (upsert key)
	create("gsc_rows", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "query", Value: 1},
			{Key: "page", Value: 1},
			{Key: "date", Value: 1},
			{Key: "device", Value: 1},
		},
		Options: options.Index().SetUnique(true).SetName("gsc_rows_unique"),
	})

	// gsc_rows: time-range aggregations per project
	create("gsc_rows", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "date", Value: 1},
		},
		Options: options.Index().SetName("gsc_rows_project_date"),
	})

	// gsc_rows: per-page lookups (opportunity engine, impact tracking)
	create("gsc_rows", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "page", Value: 1},
			{Key: "date", Value: 1},
		},
		Options: options.Index().SetName("gsc_rows_project_page_date"),
	})

	// gsc_sync_state: one state document per project
	create("gsc_sync_state", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("gsc_sync_state_project_unique"),
	})

	// opportunities: per-project listing by rank
	create("opportunities", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "rank", Value: 1},
		},
		Options: options.Index().SetName("opportunities_project_rank"),
	})

	// opportunities: filter by signal type
	create("opportunities", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "type", Value: 1},
		},
		Options: options.Index().SetName("opportunities_project_type"),
	})

	// cache_entries: one render per page; lookups by /prerender
	create("cache_entries", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "key", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("cache_entries_project_key"),
	})
	create("cache_entries", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "hits", Value: -1}},
		Options: options.Index().SetName("cache_entries_project_hits"),
	})
	// cache_entries: a render not refreshed for 30 days is removed
	create("cache_entries", mongo.IndexModel{
		Keys:    bson.D{{Key: "purge_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("cache_entries_purge_ttl"),
	})

	// prerender_data: the request log of a project over time, kept 90 days
	create("prerender_data", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "cached_at", Value: -1}},
		Options: options.Index().SetName("prerender_data_project_time"),
	})
	create("prerender_data", mongo.IndexModel{
		Keys:    bson.D{{Key: "cached_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(90 * 24 * 3600).SetName("prerender_data_ttl"),
	})

	// crawl_queue: a project's renders, kept 30 days
	create("crawl_queue", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "enqueued_at", Value: -1}},
		Options: options.Index().SetName("crawl_queue_project_time"),
	})
	create("crawl_queue", mongo.IndexModel{
		Keys:    bson.D{{Key: "enqueued_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(30 * 24 * 3600).SetName("crawl_queue_ttl"),
	})

	// page_vitals: runs of a project, kept 180 days
	create("page_vitals", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "run_at", Value: -1}},
		Options: options.Index().SetName("page_vitals_project_run"),
	})
	create("page_vitals", mongo.IndexModel{
		Keys:    bson.D{{Key: "run_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(180 * 24 * 3600).SetName("page_vitals_ttl"),
	})

	// cdn_purges: a project's recent purges, kept 30 days
	create("cdn_purges", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "at", Value: -1}},
		Options: options.Index().SetName("cdn_purges_project_time"),
	})
	create("cdn_purges", mongo.IndexModel{
		Keys:    bson.D{{Key: "at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(30 * 24 * 3600).SetName("cdn_purges_ttl"),
	})

	// digest_log: when a project's summary went out
	create("digest_log", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}, {Key: "sent_at", Value: -1}},
		Options: options.Index().SetName("digest_log_project_time"),
	})

	// opportunities: one row per finding, kept across recomputations
	create("opportunities", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "key", Value: 1},
		},
		Options: options.Index().SetUnique(true).SetName("opportunities_project_key").
			SetPartialFilterExpression(bson.M{"key": bson.M{"$type": "string"}}),
	})

	// opportunities: a page's signals (page analysis, Site Health)
	create("opportunities", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "page", Value: 1},
		},
		Options: options.Index().SetName("opportunities_project_page"),
	})

	// suggestions: per-project listing by status
	create("suggestions", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "status", Value: 1},
			{Key: "created_at", Value: -1},
		},
		Options: options.Index().SetName("suggestions_project_status"),
	})

	// suggestions: pending lookups per page (analysis skip rules)
	create("suggestions", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "page", Value: 1},
			{Key: "status", Value: 1},
		},
		Options: options.Index().SetName("suggestions_project_page_status"),
	})

	// page_analyses: per-project listing and latest analysis of a page
	create("page_analyses", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "created_at", Value: -1},
		},
		Options: options.Index().SetName("page_analyses_project_date"),
	})
	create("page_analyses", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "page", Value: 1},
			{Key: "created_at", Value: -1},
		},
		Options: options.Index().SetName("page_analyses_project_page_date"),
	})

	// page_snapshots: one snapshot per project × URL
	create("page_snapshots", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "url", Value: 1},
		},
		Options: options.Index().SetUnique(true).SetName("page_snapshots_project_url_unique"),
	})

	// page_snapshots: expire stale snapshots
	create("page_snapshots", mongo.IndexModel{
		Keys:    bson.D{{Key: "expires_at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("page_snapshots_ttl"),
	})

	// suggestion_runs: one run marker per project
	create("suggestion_runs", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("suggestion_runs_project_unique"),
	})

	// ga4_rows: unique row per project × page × date (upsert key)
	create("ga4_rows", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "page", Value: 1},
			{Key: "date", Value: 1},
		},
		Options: options.Index().SetUnique(true).SetName("ga4_rows_unique"),
	})

	// ga4_rows: time-range aggregations per project
	create("ga4_rows", mongo.IndexModel{
		Keys: bson.D{
			{Key: "project_id", Value: 1},
			{Key: "date", Value: 1},
		},
		Options: options.Index().SetName("ga4_rows_project_date"),
	})

	// ga4_sync_state: one state document per project
	create("ga4_sync_state", mongo.IndexModel{
		Keys:    bson.D{{Key: "project_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("ga4_sync_state_project_unique"),
	})
}

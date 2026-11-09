package worker

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestClearCrawlDataStaysInItsProject(t *testing.T) {
	database := integrationDB(t)
	ctx := context.Background()
	mine, other := bson.NewObjectID(), bson.NewObjectID()
	read := time.Now().UTC()

	for _, id := range []bson.ObjectID{mine, other} {
		for _, name := range crawlDerivedCollections {
			if _, err := database.Collection(name).InsertOne(ctx, bson.M{"project_id": id, "url": name}); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"gsc_rows", "ga4_rows", "prerender_data", "page_vitals"} {
			if _, err := database.Collection(name).InsertOne(ctx, bson.M{"project_id": id}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := database.Collection("site_urls").InsertOne(ctx,
			bson.M{"project_id": id, "url": "https://a.test/x", "last_fetched_at": read, "last_error": "e"}); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := ClearCrawlData(ctx, database, mine)
	if err != nil {
		t.Fatal(err)
	}
	count := func(name string, id bson.ObjectID) int64 {
		n, cErr := database.Collection(name).CountDocuments(ctx, bson.M{"project_id": id})
		if cErr != nil {
			t.Fatal(cErr)
		}
		return n
	}
	for _, name := range crawlDerivedCollections {
		if deleted[name] != 1 || count(name, mine) != 0 {
			t.Errorf("%s: deleted %d, left %d", name, deleted[name], count(name, mine))
		}
		if count(name, other) != 1 {
			t.Errorf("%s: the other project lost its data", name)
		}
	}
	for _, name := range []string{"gsc_rows", "ga4_rows", "prerender_data", "page_vitals", "site_urls"} {
		if count(name, mine) != 1 {
			t.Errorf("%s should be kept", name)
		}
	}
	urlState := func(id bson.ObjectID) bson.M {
		var doc bson.M
		if fErr := database.Collection("site_urls").FindOne(ctx, bson.M{"project_id": id}).Decode(&doc); fErr != nil {
			t.Fatal(fErr)
		}
		return doc
	}
	if doc := urlState(mine); doc["last_fetched_at"] != nil || doc["last_error"] != nil {
		t.Errorf("the URL should count as unread: %v", doc)
	}
	if urlState(other)["last_fetched_at"] == nil {
		t.Error("the other project's URL should stay read")
	}
}

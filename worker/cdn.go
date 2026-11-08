package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// Why a purge was made.
const (
	CDNTriggerTask   = "task"
	CDNTriggerManual = "manual"
)

const (
	// cdnPurgeMax caps the URLs of one purge; a larger change can purge the
	// whole zone by hand.
	cdnPurgeMax      = 500
	cdnPurgeParallel = 5
)

// bunnyAPIBase is a variable so that tests can point it at a stub.
var bunnyAPIBase = "https://api.bunny.net"

var cdnClient = &http.Client{Timeout: 20 * time.Second}

// ErrCDNKeyRefused means the CDN turned the API key down.
var ErrCDNKeyRefused = errors.New("the CDN refused the API key")

// ErrCDNZoneNotFound means the CDN has no such pull zone.
var ErrCDNZoneNotFound = errors.New("the CDN has no pull zone with this ID")

func bunnyDo(ctx context.Context, method, path, key string, q url.Values) error {
	endpoint := bunnyAPIBase + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("AccessKey", key)
	req.Header.Set("Accept", "application/json")
	resp, err := cdnClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrCDNKeyRefused
	case resp.StatusCode == http.StatusNotFound:
		return ErrCDNZoneNotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("bunny answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// CheckCDN confirms that the CDN accepts the settings.
func CheckCDN(ctx context.Context, cfg models.ProjectCDN) error {
	if cfg.PullZoneID > 0 {
		return bunnyDo(ctx, http.MethodGet, fmt.Sprintf("/pullzone/%d", cfg.PullZoneID), cfg.APIKey, nil)
	}
	return bunnyDo(ctx, http.MethodGet, "/pullzone", cfg.APIKey, url.Values{"page": {"1"}, "perPage": {"5"}})
}

// bunnyPurgeURL purges one URL. exactPath matters: without it Bunny adds a
// wildcard to a URL ending in "/", and purging the home page would empty the
// whole site.
func bunnyPurgeURL(ctx context.Context, key, pageURL string) error {
	return bunnyDo(ctx, http.MethodPost, "/purge", key, url.Values{
		"url": {pageURL}, "async": {"true"}, "exactPath": {"true"},
	})
}

// PurgeCDN removes pages from the project's CDN. Without a CDN it does nothing.
func PurgeCDN(ctx context.Context, db *mongo.Database, project *models.Project, urls []string, trigger string) (*models.CDNPurge, error) {
	cfg := project.CDN
	if cfg.Provider != models.CDNProviderBunny || cfg.APIKey == "" || len(urls) == 0 {
		return nil, nil
	}
	record := models.CDNPurge{ID: bson.NewObjectID(), ProjectID: project.ID, At: time.Now().UTC(), Trigger: trigger, URLs: len(urls)}
	if len(urls) > cdnPurgeMax {
		record.Skipped = len(urls) - cdnPurgeMax
		urls = urls[:cdnPurgeMax]
		record.URLs = len(urls)
	}
	var failed atomic.Int32
	var firstErr atomic.Value
	var wg sync.WaitGroup
	slots := make(chan struct{}, cdnPurgeParallel)
	for _, u := range urls {
		wg.Add(1)
		slots <- struct{}{}
		go func(page string) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := bunnyPurgeURL(ctx, cfg.APIKey, page); err != nil {
				failed.Add(1)
				firstErr.CompareAndSwap(nil, err.Error())
			}
		}(u)
	}
	wg.Wait()
	record.Failed = int(failed.Load())
	var err error
	if v := firstErr.Load(); v != nil {
		record.Error = v.(string)
		err = errors.New(record.Error)
	}
	saveCDNPurge(ctx, db, &record)
	return &record, err
}

// PurgeCDNZone empties the project's whole pull zone.
func PurgeCDNZone(ctx context.Context, db *mongo.Database, project *models.Project) (*models.CDNPurge, error) {
	cfg := project.CDN
	if cfg.Provider != models.CDNProviderBunny || cfg.APIKey == "" {
		return nil, errors.New("no CDN is connected")
	}
	if cfg.PullZoneID <= 0 {
		return nil, errors.New("purging everything needs the pull zone ID")
	}
	record := models.CDNPurge{ID: bson.NewObjectID(), ProjectID: project.ID, At: time.Now().UTC(), Trigger: CDNTriggerManual, All: true}
	err := bunnyDo(ctx, http.MethodPost, fmt.Sprintf("/pullzone/%d/purgeCache", cfg.PullZoneID), cfg.APIKey, nil)
	if err != nil {
		record.Failed, record.Error = 1, err.Error()
	}
	saveCDNPurge(ctx, db, &record)
	return &record, err
}

func saveCDNPurge(ctx context.Context, db *mongo.Database, record *models.CDNPurge) {
	_, _ = db.Collection("cdn_purges").InsertOne(context.WithoutCancel(ctx), record)
}

// purgeProjectCDN purges pages when the project has a CDN. A failure is
// logged and recorded: it must not stop the work that triggered it.
func (p *Processor) purgeProjectCDN(ctx context.Context, projectID bson.ObjectID, urls []string, trigger string) {
	var project models.Project
	if err := p.db.Collection("projects").FindOne(ctx, bson.M{"_id": projectID}).Decode(&project); err != nil {
		return
	}
	if _, err := PurgeCDN(ctx, p.db, &project, urls, trigger); err != nil {
		p.logger.Warn("CDN purge failed", zap.String("project_id", projectID.Hex()), zap.Error(err))
	}
}

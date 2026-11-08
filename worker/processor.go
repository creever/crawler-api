package worker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

// Processor holds the dependencies needed to execute crawl tasks.
type Processor struct {
	db          *mongo.Database
	logger      *zap.Logger
	crawlerAddr string
	httpClient  *http.Client
	asynqClient *asynq.Client
}

// NewProcessor creates a Processor wired to the given MongoDB database,
// crawler base URL (e.g. "http://crawler"), and asynq client for follow-up
// tasks.
func NewProcessor(db *mongo.Database, logger *zap.Logger, crawlerAddr string, asynqClient *asynq.Client) *Processor {
	return &Processor{
		db:          db,
		logger:      logger,
		crawlerAddr: crawlerAddr,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		asynqClient: asynqClient,
	}
}

// Register mounts the task handlers onto the provided ServeMux.
func (p *Processor) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(TypeCrawlRender, p.HandleCrawlRender)
	mux.HandleFunc(TypeBlogGenerate, p.HandleBlogGenerate)
	mux.HandleFunc(TypeGSCSync, p.HandleGSCSync)
	mux.HandleFunc(TypeComputeOpps, p.HandleComputeOpportunities)
	mux.HandleFunc(TypeGenSuggest, p.HandleSuggestionsGenerate)
	mux.HandleFunc(TypeVerifyTask, p.HandleTaskVerify)
	mux.HandleFunc(TypeMeasureTasks, p.HandleTasksMeasure)
	mux.HandleFunc(TypeSiteCrawl, p.HandleSiteCrawl)
	mux.HandleFunc(TypeSiteVitals, p.HandleSiteVitals)
	mux.HandleFunc(TypeDigestSend, p.HandleDigestSend)
	mux.HandleFunc("ga4:sync", p.HandleGA4Sync)
}

// NewServer creates a pre-configured asynq.Server backed by Redis at redisAddr.
func NewServer(redisAddr string, logger *zap.Logger) *asynq.Server {
	return asynq.NewServer(
		asynq.RedisClientOpt{Addr: redisAddr},
		asynq.Config{
			Concurrency: 5,
			// Cache refreshes are short and keep what bots get current; they
			// go before long crawls and analyses.
			Queues: map[string]int{queueCritical: 6, "default": 3},
			ErrorHandler: asynq.ErrorHandlerFunc(func(ctx context.Context, task *asynq.Task, err error) {
				logger.Error("asynq task failed",
					zap.String("type", task.Type()),
					zap.Error(err),
				)
			}),
		},
	)
}

// renderedPage is a page as the crawler's /render returned it.
type renderedPage struct {
	HTML       string
	Status     int
	FinalURL   string
	Redirects  int
	XRobotsTag string
	RenderMs   int64
}

// fetchPage calls GET /render?url={u}&js={useJS}. The page's status, final
// URL, redirect count and robots header come in X- headers: an error status
// is a normal answer, with the page's body.
func (p *Processor) fetchPage(ctx context.Context, rawURL string, useJS bool) (*renderedPage, error) {
	endpoint := fmt.Sprintf("%s/render?url=%s&js=%v", p.crawlerAddr, url.QueryEscape(rawURL), useJS)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build render request: %w", err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("render request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crawler /render returned HTTP %s for %s", resp.Status, rawURL)
	}
	page := &renderedPage{Status: http.StatusOK, FinalURL: rawURL, XRobotsTag: resp.Header.Get("X-Origin-Robots-Tag")}
	if v, convErr := strconv.Atoi(resp.Header.Get("X-Prerender-Status")); convErr == nil && v > 0 {
		page.Status = v
	}
	if v := resp.Header.Get("X-Final-URL"); v != "" {
		page.FinalURL = v
	}
	page.Redirects, _ = strconv.Atoi(resp.Header.Get("X-Redirect-Count"))
	page.RenderMs, _ = strconv.ParseInt(resp.Header.Get("X-Render-Ms"), 10, 64)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read render response: %w", err)
	}
	page.HTML = string(body)
	return page, nil
}

// setQueueStatus records a render queue entry's progress.
func (p *Processor) setQueueStatus(ctx context.Context, queueEntryID string, status models.QueueStatus, errMsg string) {
	oid, err := bson.ObjectIDFromHex(queueEntryID)
	if err != nil {
		p.logger.Warn("setQueueStatus: invalid queue_entry_id", zap.String("id", queueEntryID))
		return
	}

	update := bson.M{"status": status}
	if errMsg != "" {
		update["error"] = errMsg
	}
	if status == models.QueueStatusDone || status == models.QueueStatusFailed {
		now := time.Now().UTC()
		update["processed_at"] = now
	}

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if _, err = p.db.Collection("crawl_queue").UpdateOne(
		tctx,
		bson.M{"_id": oid},
		bson.M{"$set": update},
	); err != nil {
		p.logger.Error("setQueueStatus: failed to update queue entry",
			zap.String("id", queueEntryID),
			zap.Error(err),
		)
	}
}

package handlers

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

const (
	aiTopPages       = 25
	aiSearchPages    = 50
	aiMissingListed  = 10
	aiDefaultDays    = 30
	aiPageAgentLimit = 4
)

// aiAgentRow is one AI crawler: whether it may come and what it did.
type aiAgentRow struct {
	Agent    string     `json:"agent"`
	Allowed  *bool      `json:"allowed"` // nil before the first crawl
	Requests int64      `json:"requests"`
	Pages    int64      `json:"pages"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// aiPageRow is a page AI crawlers ask for.
type aiPageRow struct {
	URL         string             `json:"url"`
	Requests    int64              `json:"requests"`
	Agents      []string           `json:"agents"`
	Status      int                `json:"status"`
	Title       string             `json:"title"`
	Read        bool               `json:"read"`
	Issues      []models.PageIssue `json:"issues"`
	Impressions float64            `json:"impressions"`
}

// aiMissingPage is a strong search page no AI crawler has asked for.
type aiMissingPage struct {
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Impressions float64 `json:"impressions"`
}

type aiVisibility struct {
	Days        int                    `json:"days"`
	Requests    int64                  `json:"requests"`
	Rendering   *models.RenderingProbe `json:"rendering"`
	Agents      []aiAgentRow           `json:"agents"`
	Pages       []aiPageRow            `json:"pages"`
	SearchPages int                    `json:"search_pages"` // strongest search pages looked at
	AskedByAI   int                    `json:"asked_by_ai"`  // of those, asked for by an AI crawler
	Missing     []aiMissingPage        `json:"missing"`
}

// AIVisibility godoc
// @Summary      What AI crawlers may fetch, do fetch and get to see
// @Tags         prerender
// @Produce      json
// @Param        id    path   string  true   "Project ID"
// @Param        days  query  int     false  "Days to cover (default 30, max 90)"
// @Router       /api/v1/projects/{id}/ai-visibility [get]
func (h *PrerenderHandler) AIVisibility(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(c.DefaultQuery("days", strconv.Itoa(aiDefaultDays)))
	if days <= 0 || days > prerenderStatsMaxDays {
		days = aiDefaultDays
	}
	out, err := buildAIVisibility(ctx, h.db, id, days, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build the AI visibility report"})
		return
	}
	c.JSON(http.StatusOK, out)
}

func buildAIVisibility(ctx context.Context, db *mongo.Database, id bson.ObjectID, days int, now time.Time) (*aiVisibility, error) {
	out := &aiVisibility{Days: days, Agents: []aiAgentRow{}, Pages: []aiPageRow{}, Missing: []aiMissingPage{}}
	audit, err := worker.LatestAudit(ctx, db, id)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	if audit != nil {
		out.Rendering = audit.Rendering
		for _, a := range audit.AIAccess {
			allowed[a.Agent] = a.Allowed
		}
	}

	match := bson.D{{Key: "$match", Value: bson.M{
		"project_id": id, "requester_type": models.RequesterTypeAI,
		"cached_at": bson.M{"$gte": now.AddDate(0, 0, -days+1).Truncate(24 * time.Hour)},
	}}}
	var rows []struct {
		ID struct {
			Key   string `bson:"key"`
			Agent string `bson:"agent"`
		} `bson:"_id"`
		URL    string    `bson:"url"`
		N      int64     `bson:"n"`
		Status int       `bson:"status"`
		Last   time.Time `bson:"last"`
	}
	cursor, err := db.Collection("prerender_data").Aggregate(ctx, mongo.Pipeline{match,
		{{Key: "$sort", Value: bson.M{"cached_at": 1}}},
		{{Key: "$group", Value: bson.M{
			"_id":    bson.M{"key": bson.M{"$ifNull": bson.A{"$key", "$url"}}, "agent": bson.M{"$ifNull": bson.A{"$agent", ""}}},
			"url":    bson.M{"$last": "$url"},
			"n":      bson.M{"$sum": 1},
			"status": bson.M{"$last": "$status_code"},
			"last":   bson.M{"$last": "$cached_at"},
		}}},
	})
	if err != nil {
		return nil, err
	}
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err
	}

	agents := make(map[string]*aiAgentRow)
	pages := make(map[string]*aiPageRow)
	agentHits := make(map[string]map[string]int64)
	for _, r := range rows {
		out.Requests += r.N
		name := r.ID.Agent
		if name == "" {
			name = "Other AI crawler"
		}
		a := agents[name]
		if a == nil {
			a = &aiAgentRow{Agent: name}
			agents[name] = a
		}
		a.Requests += r.N
		a.Pages++
		if last := r.Last; a.LastSeen == nil || last.After(*a.LastSeen) {
			a.LastSeen = &last
		}
		key := worker.NormalizePageURL(r.URL)
		pg := pages[key]
		if pg == nil {
			pg = &aiPageRow{URL: r.URL, Status: r.Status, Issues: []models.PageIssue{}}
			pages[key] = pg
			agentHits[key] = map[string]int64{}
		}
		pg.Requests += r.N
		agentHits[key][name] += r.N
	}
	// Every crawler robots.txt was read for appears, also with no requests.
	for agent := range allowed {
		if agents[agent] == nil {
			agents[agent] = &aiAgentRow{Agent: agent}
		}
	}
	for name, a := range agents {
		if v, known := allowed[name]; known {
			v := v
			a.Allowed = &v
		}
		out.Agents = append(out.Agents, *a)
	}
	sort.Slice(out.Agents, func(i, j int) bool {
		if out.Agents[i].Requests != out.Agents[j].Requests {
			return out.Agents[i].Requests > out.Agents[j].Requests
		}
		return out.Agents[i].Agent < out.Agents[j].Agent
	})

	keys := make([]string, 0, len(pages))
	for k := range pages {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if pages[keys[i]].Requests != pages[keys[j]].Requests {
			return pages[keys[i]].Requests > pages[keys[j]].Requests
		}
		return keys[i] < keys[j]
	})
	if len(keys) > aiTopPages {
		keys = keys[:aiTopPages]
	}
	lookup := make(bson.A, 0, len(keys))
	for _, k := range keys {
		lookup = append(lookup, k)
	}
	profiles := map[string]models.PageProfile{}
	if len(lookup) > 0 {
		pc, findErr := db.Collection("page_profiles").Find(ctx, bson.M{"project_id": id, "url": bson.M{"$in": lookup}, "version": bson.M{"$gte": 1}},
			options.Find().SetProjection(bson.M{"url": 1, "title": 1, "issues": 1, "impressions": 1}))
		if findErr != nil {
			return nil, findErr
		}
		var found []models.PageProfile
		if err = pc.All(ctx, &found); err != nil {
			return nil, err
		}
		for _, p := range found {
			profiles[p.URL] = p
		}
	}
	for _, k := range keys {
		pg := pages[k]
		pg.Agents = topAgents(agentHits[k], aiPageAgentLimit)
		if pr, read := profiles[k]; read {
			pg.Read, pg.Title, pg.Impressions = true, pr.Title, pr.Impressions
			if pr.Issues != nil {
				pg.Issues = pr.Issues
			}
		}
		out.Pages = append(out.Pages, *pg)
	}

	// Of the pages that matter most in search, which have AI crawlers seen?
	sc, err := db.Collection("page_profiles").Find(ctx, bson.M{"project_id": id, "version": bson.M{"$gte": 1}, "impressions": bson.M{"$gt": 0}, "status_code": bson.M{"$lt": 400}},
		options.Find().SetSort(bson.D{{Key: "impressions", Value: -1}}).SetLimit(aiSearchPages).SetProjection(bson.M{"url": 1, "source_url": 1, "title": 1, "impressions": 1}))
	if err != nil {
		return nil, err
	}
	var strongest []models.PageProfile
	if err = sc.All(ctx, &strongest); err != nil {
		return nil, err
	}
	out.SearchPages = len(strongest)
	for _, pr := range strongest {
		if pages[pr.URL] != nil {
			out.AskedByAI++
		} else if len(out.Missing) < aiMissingListed {
			out.Missing = append(out.Missing, aiMissingPage{URL: pr.SourceURL, Title: pr.Title, Impressions: pr.Impressions})
		}
	}
	return out, nil
}

// topAgents names the crawlers that asked for a page most.
func topAgents(hits map[string]int64, n int) []string {
	names := make([]string, 0, len(hits))
	for name := range hits {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if hits[names[i]] != hits[names[j]] {
			return hits[names[i]] > hits[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > n {
		names = names[:n]
	}
	return names
}

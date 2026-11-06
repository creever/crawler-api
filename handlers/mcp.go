package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// MCPHandler exposes the crawler-api as an MCP (Model Context Protocol)
// endpoint so it can be added to claude.ai as a custom connector.
// It speaks JSON-RPC 2.0 over HTTP POST /mcp with bearer-token auth.
type MCPHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

// NewMCPHandler creates a new MCPHandler.
func NewMCPHandler(db *mongo.Database, asynqClient *asynq.Client) *MCPHandler {
	return &MCPHandler{db: db, asynqClient: asynqClient}
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{
		Name:        "gsc_query",
		Description: "Top Google Search Console queries for a project over the last N days (clicks, impressions, CTR, position), aggregated from synced gsc_rows.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project_id": map[string]interface{}{"type": "string", "description": "Project ID"},
				"days":       map[string]interface{}{"type": "integer", "description": "Lookback days (default 28)"},
				"limit":      map[string]interface{}{"type": "integer", "description": "Max rows (default 50)"},
			},
			"required": []string{"project_id"},
		},
	},
	{
		Name:        "opportunities",
		Description: "Ranked organic-search opportunities for a project (striking_distance, low_ctr, losing_clicks, cannibalization, crawl_issue).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project_id": map[string]interface{}{"type": "string"},
				"type":       map[string]interface{}{"type": "string", "description": "Optional signal type filter"},
				"limit":      map[string]interface{}{"type": "integer", "description": "Max rows (default 50)"},
			},
			"required": []string{"project_id"},
		},
	},
	{
		Name:        "ga4_report",
		Description: "GA4 landing-page report for a project: sessions, conversions, revenue over the last N days.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project_id": map[string]interface{}{"type": "string"},
				"days":       map[string]interface{}{"type": "integer", "description": "Lookback days (default 28)"},
			},
			"required": []string{"project_id"},
		},
	},
	{
		Name:        "start_crawl",
		Description: "Enqueue an SEO crawl task for a project URL (rate-limited: one crawl per project per 10 minutes).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project_id": map[string]interface{}{"type": "string"},
				"url":        map[string]interface{}{"type": "string", "description": "Defaults to the project URL"},
			},
			"required": []string{"project_id"},
		},
	},
	{
		Name:        "list_suggestions",
		Description: "List AI fix suggestions for a project, optionally filtered by status (pending, approved, rejected, edited).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"project_id": map[string]interface{}{"type": "string"},
				"status":     map[string]interface{}{"type": "string"},
				"limit":      map[string]interface{}{"type": "integer", "description": "Max rows (default 50)"},
			},
			"required": []string{"project_id"},
		},
	},
}

// Handle godoc
// @Summary      MCP endpoint (JSON-RPC 2.0 over HTTP)
// @Description  Supports initialize, ping, tools/list and tools/call. Auth: Authorization: Bearer <token> (SHA-256 hash stored in settings as mcp_token_hash).
// @Tags         mcp
// @Router       /mcp [post]
func (h *MCPHandler) Handle(c *gin.Context) {
	var req mcpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"jsonrpc": "2.0", "id": nil, "error": map[string]interface{}{"code": -32700, "message": "parse error"}})
		return
	}

	switch req.Method {
	case "initialize":
		c.JSON(http.StatusOK, gin.H{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": gin.H{
				"protocolVersion": "2025-03-26",
				"capabilities":    gin.H{"tools": gin.H{"listChanged": false}},
				"serverInfo":      gin.H{"name": "creever-crawler-api", "version": "1.0.0"},
			},
		})
	case "ping":
		c.JSON(http.StatusOK, gin.H{"jsonrpc": "2.0", "id": req.ID, "result": gin.H{}})
	case "tools/list":
		c.JSON(http.StatusOK, gin.H{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  gin.H{"tools": mcpTools},
		})
	case "tools/call":
		h.handleToolCall(c, req)
	default:
		// Notifications (no id) get no response body per JSON-RPC.
		if req.ID == nil {
			c.Status(http.StatusAccepted)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error":   map[string]interface{}{"code": -32601, "message": "method not found: " + req.Method},
		})
	}
}

func jsonUnmarshal(data []byte, v interface{}) error {
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

func jsonMarshalIndent(v interface{}) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func (h *MCPHandler) handleToolCall(c *gin.Context, req mcpRequest) {
	var params struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := jsonUnmarshal(req.Params, &params); err != nil || params.Name == "" {
		mcpToolError(c, req.ID, -32602, "invalid params: expected {name, arguments}")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result interface{}
	var toolErr string
	switch params.Name {
	case "gsc_query":
		result, toolErr = h.toolGSCQuery(ctx, params.Arguments)
	case "opportunities":
		result, toolErr = h.toolOpportunities(ctx, params.Arguments)
	case "ga4_report":
		result, toolErr = h.toolGA4Report(ctx, params.Arguments)
	case "start_crawl":
		result, toolErr = h.toolStartCrawl(ctx, params.Arguments)
	case "list_suggestions":
		result, toolErr = h.toolListSuggestions(ctx, params.Arguments)
	default:
		mcpToolError(c, req.ID, -32602, "unknown tool: "+params.Name)
		return
	}

	if toolErr != "" {
		mcpToolResult(c, req.ID, toolErr, true)
		return
	}
	mcpToolResult(c, req.ID, result, false)
}

func mcpToolError(c *gin.Context, id interface{}, code int, msg string) {
	c.JSON(http.StatusOK, gin.H{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]interface{}{"code": code, "message": msg},
	})
}

func mcpToolResult(c *gin.Context, id interface{}, content interface{}, isError bool) {
	text, _ := jsonMarshalIndent(content)
	c.JSON(http.StatusOK, gin.H{
		"jsonrpc": "2.0",
		"id":      id,
		"result": gin.H{
			"content": []map[string]interface{}{{"type": "text", "text": string(text)}},
			"isError": isError,
		},
	})
}

func (h *MCPHandler) argProjectID(args map[string]interface{}) (bson.ObjectID, string) {
	pid, _ := args["project_id"].(string)
	oid, err := bson.ObjectIDFromHex(pid)
	if err != nil {
		return bson.NilObjectID, "invalid project_id"
	}
	return oid, ""
}

func argInt(args map[string]interface{}, key string, def int) int {
	if v, ok := args[key].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

func (h *MCPHandler) toolGSCQuery(ctx context.Context, args map[string]interface{}) (interface{}, string) {
	oid, errMsg := h.argProjectID(args)
	if errMsg != "" {
		return nil, errMsg
	}
	days := argInt(args, "days", 28)
	limit := argInt(args, "limit", 50)
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: oid},
			{Key: "date", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$query"},
			{Key: "clicks", Value: bson.D{{Key: "$sum", Value: "$clicks"}}},
			{Key: "impressions", Value: bson.D{{Key: "$sum", Value: "$impressions"}}},
			{Key: "pos_weighted", Value: bson.D{{Key: "$sum", Value: bson.D{{Key: "$multiply", Value: bson.A{"$position", "$impressions"}}}}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "clicks", Value: -1}}}},
		{{Key: "$limit", Value: limit}},
	}
	cursor, err := h.db.Collection("gsc_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err.Error()
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []bson.M
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err.Error()
	}
	return gin.H{"days": days, "rows": rows}, ""
}

func (h *MCPHandler) toolOpportunities(ctx context.Context, args map[string]interface{}) (interface{}, string) {
	oid, errMsg := h.argProjectID(args)
	if errMsg != "" {
		return nil, errMsg
	}
	limit := argInt(args, "limit", 50)
	query := bson.M{"project_id": oid}
	if t, ok := args["type"].(string); ok && t != "" {
		query["type"] = models.OpportunityType(t)
	}
	cursor, err := h.db.Collection("opportunities").Find(ctx, query,
		options.Find().SetSort(bson.D{{Key: "rank", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err.Error()
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []models.Opportunity
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err.Error()
	}
	return gin.H{"count": len(rows), "opportunities": rows}, ""
}

func (h *MCPHandler) toolGA4Report(ctx context.Context, args map[string]interface{}) (interface{}, string) {
	oid, errMsg := h.argProjectID(args)
	if errMsg != "" {
		return nil, errMsg
	}
	days := argInt(args, "days", 28)
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "project_id", Value: oid},
			{Key: "date", Value: bson.D{{Key: "$gte", Value: since}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$page"},
			{Key: "sessions", Value: bson.D{{Key: "$sum", Value: "$sessions"}}},
			{Key: "conversions", Value: bson.D{{Key: "$sum", Value: "$conversions"}}},
			{Key: "revenue", Value: bson.D{{Key: "$sum", Value: "$revenue"}}},
		}}},
		{{Key: "$sort", Value: bson.D{{Key: "revenue", Value: -1}}}},
		{{Key: "$limit", Value: 50}},
	}
	cursor, err := h.db.Collection("ga4_rows").Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err.Error()
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []bson.M
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err.Error()
	}
	return gin.H{"days": days, "rows": rows}, ""
}

func (h *MCPHandler) toolStartCrawl(ctx context.Context, args map[string]interface{}) (interface{}, string) {
	oid, errMsg := h.argProjectID(args)
	if errMsg != "" {
		return nil, errMsg
	}
	var project models.Project
	if err := h.db.Collection("projects").FindOne(ctx, bson.M{"_id": oid}).Decode(&project); err != nil {
		return nil, "project not found"
	}

	// Rate-limit: one crawl per project per 10 minutes.
	recent, err := h.db.Collection("crawl_queue").CountDocuments(ctx, bson.M{
		"project_id":  oid,
		"enqueued_at": bson.M{"$gte": time.Now().UTC().Add(-10 * time.Minute)},
	})
	if err == nil && recent > 0 {
		return nil, "rate limited: a crawl for this project was enqueued in the last 10 minutes"
	}

	url, _ := args["url"].(string)
	if url == "" {
		url = project.URL
	}

	entryID := bson.NewObjectID()
	entry := models.QueueEntry{
		ID:         entryID,
		ProjectID:  oid,
		URL:        url,
		TaskType:   models.QueueTaskTypeSEO,
		Status:     models.QueueStatusPending,
		EnqueuedAt: time.Now().UTC(),
	}
	task, err := worker.NewCrawlSEOTask(entryID.Hex(), models.ProjectConfig{
		ProjectID: oid.Hex(),
		SeedURLs:  []string{url},
		UseJS:     project.UseJS,
		MaxPages:  1,
	})
	if err != nil {
		return nil, err.Error()
	}
	info, err := h.asynqClient.EnqueueContext(ctx, task)
	if err != nil {
		return nil, err.Error()
	}
	entry.AsynqTaskID = info.ID
	if _, err = h.db.Collection("crawl_queue").InsertOne(ctx, entry); err != nil {
		return nil, err.Error()
	}
	return gin.H{"enqueued": true, "queue_entry_id": entryID.Hex(), "url": url}, ""
}

func (h *MCPHandler) toolListSuggestions(ctx context.Context, args map[string]interface{}) (interface{}, string) {
	oid, errMsg := h.argProjectID(args)
	if errMsg != "" {
		return nil, errMsg
	}
	limit := argInt(args, "limit", 50)
	query := bson.M{"project_id": oid}
	if s, ok := args["status"].(string); ok && s != "" {
		query["status"] = models.SuggestionStatus(s)
	}
	cursor, err := h.db.Collection("suggestions").Find(ctx, query,
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err.Error()
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var rows []models.Suggestion
	if err = cursor.All(ctx, &rows); err != nil {
		return nil, err.Error()
	}
	return gin.H{"count": len(rows), "suggestions": rows}, ""
}

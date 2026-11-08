package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

const cdnManualPurgeMax = 100

// CDNHandler connects a project to its CDN and purges it.
type CDNHandler struct {
	db *mongo.Database
}

// NewCDNHandler creates a CDNHandler.
func NewCDNHandler(db *mongo.Database) *CDNHandler { return &CDNHandler{db: db} }

func (h *CDNHandler) project(ctx context.Context, c *gin.Context) (*models.Project, bool) {
	id, ok := projectFromRequest(ctx, c, h.db)
	if !ok {
		return nil, false
	}
	var project models.Project
	if err := h.db.Collection("projects").FindOne(ctx, bson.M{"_id": id}).Decode(&project); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		return nil, false
	}
	return &project, true
}

// Get godoc
// @Summary      The project's CDN connection and recent purges; the key is never returned
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/cdn [get]
func (h *CDNHandler) Get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	project, ok := h.project(ctx, c)
	if !ok {
		return
	}
	purges := []models.CDNPurge{}
	if cursor, err := h.db.Collection("cdn_purges").Find(ctx, bson.M{"project_id": project.ID},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(10)); err == nil {
		_ = cursor.All(ctx, &purges)
	}
	c.JSON(http.StatusOK, gin.H{
		"provider": project.CDN.Provider, "connected": project.CDN.APIKey != "",
		"pull_zone_id": project.CDN.PullZoneID, "purges": purges,
	})
}

// Update godoc
// @Summary      Connect the project to a CDN, or disconnect it
// @Description  The settings are tested against the CDN before they are saved. An empty api_key keeps the stored key; an empty provider disconnects.
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/cdn [put]
func (h *CDNHandler) Update(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project, ok := h.project(ctx, c)
	if !ok {
		return
	}
	var body struct {
		Provider   string `json:"provider"`
		APIKey     string `json:"api_key"`
		PullZoneID int64  `json:"pull_zone_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}
	col := h.db.Collection("projects")
	if body.Provider == "" {
		if _, err := col.UpdateOne(ctx, bson.M{"_id": project.ID}, bson.M{"$unset": bson.M{"cdn": ""}}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disconnect the CDN"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"provider": "", "connected": false, "pull_zone_id": 0})
		return
	}
	if body.Provider != models.CDNProviderBunny {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown CDN provider"})
		return
	}
	cfg := models.ProjectCDN{Provider: body.Provider, APIKey: strings.TrimSpace(body.APIKey), PullZoneID: body.PullZoneID}
	if cfg.APIKey == "" {
		cfg.APIKey = project.CDN.APIKey
	}
	if cfg.APIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "api_key is required"})
		return
	}
	if err := worker.CheckCDN(ctx, cfg); err != nil {
		switch {
		case errors.Is(err, worker.ErrCDNKeyRefused):
			c.JSON(http.StatusBadRequest, gin.H{"error": "Bunny refused this API key. Use the account API key from the Bunny dashboard."})
		case errors.Is(err, worker.ErrCDNZoneNotFound):
			c.JSON(http.StatusBadRequest, gin.H{"error": "Bunny has no pull zone with this ID on this account."})
		default:
			c.JSON(http.StatusBadGateway, gin.H{"error": "could not reach Bunny: " + err.Error()})
		}
		return
	}
	if _, err := col.UpdateOne(ctx, bson.M{"_id": project.ID}, bson.M{"$set": bson.M{"cdn": cfg}}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the CDN settings"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"provider": cfg.Provider, "connected": true, "pull_zone_id": cfg.PullZoneID})
}

// Purge godoc
// @Summary      Purge pages, or everything, from the project's CDN
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/cdn/purge [post]
func (h *CDNHandler) Purge(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	project, ok := h.project(ctx, c)
	if !ok {
		return
	}
	if project.CDN.APIKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no CDN is connected to this project"})
		return
	}
	var body struct {
		URLs []string `json:"urls"`
		All  bool     `json:"all"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || (!body.All && len(body.URLs) == 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "urls or all is required"})
		return
	}
	if body.All {
		record, err := worker.PurgeCDNZone(ctx, h.db, project)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, record)
		return
	}
	if len(body.URLs) > cdnManualPurgeMax {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at most " + strconv.Itoa(cdnManualPurgeMax) + " URLs at once"})
		return
	}
	host := hostOf(project.URL)
	for _, u := range body.URLs {
		if hostOf(u) != host {
			c.JSON(http.StatusBadRequest, gin.H{"error": "not a page of this project: " + u})
			return
		}
	}
	record, err := worker.PurgeCDN(ctx, h.db, project, body.URLs, worker.CDNTriggerManual)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, record)
}

package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

// DigestHandler previews and sends a project's weekly summary.
type DigestHandler struct {
	db *mongo.Database
}

// NewDigestHandler creates a DigestHandler.
func NewDigestHandler(db *mongo.Database) *DigestHandler { return &DigestHandler{db: db} }

func (h *DigestHandler) project(ctx context.Context, c *gin.Context) (*models.Project, bool) {
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
// @Summary      The weekly summary as it would go out now
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/digest [get]
func (h *DigestHandler) Get(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	project, ok := h.project(ctx, c)
	if !ok {
		return
	}
	now := time.Now().UTC()
	data, err := worker.CollectDigest(ctx, h.db, project, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build the summary"})
		return
	}
	d := worker.RenderDigest(data)
	c.JSON(http.StatusOK, gin.H{
		"enabled": project.WeeklyDigest, "email_configured": worker.EmailConfigured(ctx, h.db),
		"recipient": worker.DigestRecipient(ctx, h.db, project), "last_sent_at": worker.LastDigestAt(ctx, h.db, project.ID),
		"subject": d.Subject, "html": d.HTML,
	})
}

// Send godoc
// @Summary      Send the weekly summary to the project owner now
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Router       /api/v1/projects/{id}/digest/send [post]
func (h *DigestHandler) Send(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	project, ok := h.project(ctx, c)
	if !ok {
		return
	}
	err := worker.SendDigest(ctx, h.db, project, time.Now().UTC())
	if errors.Is(err, worker.ErrEmailNotConfigured) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

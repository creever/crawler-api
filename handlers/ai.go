package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/creever/crawler-api/worker"
)

// AIHandler exposes AI provider/model metadata.
type AIHandler struct{}

// NewAIHandler creates a new AIHandler.
func NewAIHandler() *AIHandler { return &AIHandler{} }

// Models godoc
// @Summary      List available AI providers and models
// @Tags         ai
// @Produce      json
// @Success      200  {object}  map[string]interface{}
// @Router       /api/v1/ai/models [get]
func (h *AIHandler) Models(c *gin.Context) {
	providers := worker.AvailableModels
	defaults := map[string]string{}
	for p := range providers {
		defaults[p] = worker.DefaultModel(p)
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers, "defaults": defaults})
}

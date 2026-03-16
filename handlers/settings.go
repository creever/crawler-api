package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/creever/crawler-api/models"
)

// SettingsHandler holds dependencies for settings endpoints
type SettingsHandler struct {
	db *mongo.Database
}

// NewSettingsHandler creates a new SettingsHandler
func NewSettingsHandler(db *mongo.Database) *SettingsHandler {
	return &SettingsHandler{db: db}
}

func (h *SettingsHandler) col() *mongo.Collection {
	return h.db.Collection("settings")
}

// List godoc
// @Summary      List all settings
// @Tags         settings
// @Produce      json
// @Success      200  {array}   models.Setting
// @Router       /api/v1/settings [get]
func (h *SettingsHandler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cursor, err := h.col().Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "key", Value: 1}}))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch settings"})
		return
	}
	defer cursor.Close(ctx)

	var settings []models.Setting
	if err = cursor.All(ctx, &settings); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode settings"})
		return
	}
	if settings == nil {
		settings = []models.Setting{}
	}
	c.JSON(http.StatusOK, settings)
}

// Upsert godoc
// @Summary      Create or update a setting by key
// @Tags         settings
// @Accept       json
// @Produce      json
// @Param        setting  body      models.Setting  true  "Setting data"
// @Success      200      {object}  models.Setting
// @Router       /api/v1/settings [put]
func (h *SettingsHandler) Upsert(c *gin.Context) {
	var setting models.Setting
	if err := c.ShouldBindJSON(&setting); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	setting.UpdatedAt = time.Now().UTC()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result := h.col().FindOneAndUpdate(
		ctx,
		bson.M{"key": setting.Key},
		bson.M{"$set": setting},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	)
	if result.Err() != nil && result.Err() != mongo.ErrNoDocuments {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to upsert setting"})
		return
	}

	var saved models.Setting
	if err := result.Decode(&saved); err != nil {
		// On insert the decoded result may be empty; return the input with a new ID
		setting.ID = bson.NewObjectID()
		c.JSON(http.StatusOK, setting)
		return
	}
	c.JSON(http.StatusOK, saved)
}

// Delete godoc
// @Summary      Delete a setting by key
// @Tags         settings
// @Param        key  path  string  true  "Setting key"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/settings/{key} [delete]
func (h *SettingsHandler) Delete(c *gin.Context) {
	key := c.Param("key")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"key": key})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete setting"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "setting not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

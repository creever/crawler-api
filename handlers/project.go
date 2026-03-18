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

// ProjectHandler holds dependencies for project endpoints
type ProjectHandler struct {
	db *mongo.Database
}

// NewProjectHandler creates a new ProjectHandler
func NewProjectHandler(db *mongo.Database) *ProjectHandler {
	return &ProjectHandler{db: db}
}

func (h *ProjectHandler) col() *mongo.Collection {
	return h.db.Collection("projects")
}

// List godoc
// @Summary      List all projects
// @Tags         projects
// @Produce      json
// @Success      200  {array}   models.Project
// @Router       /api/v1/projects [get]
func (h *ProjectHandler) List(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := h.col().Find(ctx, bson.M{}, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch projects"})
		return
	}
	defer cursor.Close(ctx)

	var projects []models.Project
	if err = cursor.All(ctx, &projects); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode projects"})
		return
	}
	if projects == nil {
		projects = []models.Project{}
	}
	c.JSON(http.StatusOK, projects)
}

// Get godoc
// @Summary      Get a project by ID
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  models.Project
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/projects/{id} [get]
func (h *ProjectHandler) Get(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var project models.Project
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&project); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		return
	}
	c.JSON(http.StatusOK, project)
}

// Create godoc
// @Summary      Create a new project
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        project  body      models.Project  true  "Project data"
// @Success      201      {object}  models.Project
// @Router       /api/v1/projects [post]
func (h *ProjectHandler) Create(c *gin.Context) {
	var project models.Project
	if err := c.ShouldBindJSON(&project); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	now := time.Now().UTC()
	project.ID = bson.NewObjectID()
	project.Active = true
	project.CreatedAt = now
	project.UpdatedAt = now

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := h.col().InsertOne(ctx, project); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create project"})
		return
	}
	c.JSON(http.StatusCreated, project)
}

// Update godoc
// @Summary      Update a project
// @Tags         projects
// @Accept       json
// @Produce      json
// @Param        id       path      string                      true  "Project ID"
// @Param        project  body      models.ProjectUpdateInput   true  "Fields to update"
// @Success      200      {object}  models.Project
// @Router       /api/v1/projects/{id} [put]
func (h *ProjectHandler) Update(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	var input models.ProjectUpdateInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	input.UpdatedAt = time.Now().UTC()
	result := h.col().FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": input},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	if result.Err() != nil {
		if result.Err() == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update project"})
		return
	}

	var project models.Project
	if err = result.Decode(&project); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decode updated project"})
		return
	}
	c.JSON(http.StatusOK, project)
}

// Delete godoc
// @Summary      Delete a project
// @Tags         projects
// @Param        id  path  string  true  "Project ID"
// @Success      204
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/projects/{id} [delete]
func (h *ProjectHandler) Delete(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := h.col().DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete project"})
		return
	}
	if result.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// GetConfig godoc
// @Summary      Get the crawler configuration for a project
// @Description  Returns the ProjectConfig used when enqueuing crawl tasks for this project
// @Tags         projects
// @Produce      json
// @Param        id  path  string  true  "Project ID"
// @Success      200  {object}  models.ProjectConfig
// @Failure      404  {object}  map[string]string
// @Router       /api/v1/projects/{id}/config [get]
func (h *ProjectHandler) GetConfig(c *gin.Context) {
	id, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project id"})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var project models.Project
	if err = h.col().FindOne(ctx, bson.M{"_id": id}).Decode(&project); err != nil {
		if err == mongo.ErrNoDocuments {
			c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch project"})
		return
	}

	cfg := models.ProjectConfig{
		ProjectID: project.ID.Hex(),
		SeedURLs:  []string{project.URL},
		UseJS:     project.UseJS,
		MaxPages:  project.MaxPages,
	}
	c.JSON(http.StatusOK, cfg)
}

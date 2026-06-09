package handlers

import (
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

// ---------------------------------------------------------------------------
// BlogConfigHandler
// ---------------------------------------------------------------------------

type BlogConfigHandler struct {
	db *mongo.Database
}

func NewBlogConfigHandler(db *mongo.Database) *BlogConfigHandler {
	return &BlogConfigHandler{db: db}
}

func (h *BlogConfigHandler) col() *mongo.Collection {
	return h.db.Collection("blog_configs")
}

// Create godoc
// @Summary Create blog config for a project
// @Tags blog
// @Accept json
// @Produce json
// @Success 201 {object} models.BlogConfig
// @Router /api/v1/blog/configs [post]
func (h *BlogConfigHandler) Create(c *gin.Context) {
	var cfg models.BlogConfig
	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	cfg.ID = bson.NewObjectID()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	if cfg.DefaultLanguage == "" {
		cfg.DefaultLanguage = "English"
	}
	if cfg.DefaultTone == "" {
		cfg.DefaultTone = "Informative"
	}
	if cfg.DefaultWordCount == 0 {
		cfg.DefaultWordCount = 800
	}

	if _, err := h.col().InsertOne(c.Request.Context(), cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, cfg)
}

// List godoc
// @Summary List blog configs, optionally filtered by project_id
// @Tags blog
// @Produce json
// @Param project_id query string false "Project ID"
// @Router /api/v1/blog/configs [get]
func (h *BlogConfigHandler) List(c *gin.Context) {
	filter := bson.M{}
	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter["project_id"] = oid
	}

	cursor, err := h.col().Find(c.Request.Context(), filter, options.Find().SetSort(bson.M{"created_at": -1}))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer cursor.Close(c.Request.Context()) //nolint:errcheck

	var configs []models.BlogConfig
	if err = cursor.All(c.Request.Context(), &configs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, configs)
}

// Get godoc
// @Summary Get a blog config by ID
// @Tags blog
// @Produce json
// @Param id path string true "BlogConfig ID"
// @Router /api/v1/blog/configs/{id} [get]
func (h *BlogConfigHandler) Get(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var cfg models.BlogConfig
	if err = h.col().FindOne(c.Request.Context(), bson.M{"_id": oid}).Decode(&cfg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog config not found"})
		return
	}
	c.JSON(http.StatusOK, cfg)
}

// Update godoc
// @Summary Update a blog config
// @Tags blog
// @Accept json
// @Produce json
// @Param id path string true "BlogConfig ID"
// @Router /api/v1/blog/configs/{id} [put]
func (h *BlogConfigHandler) Update(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var body map[string]interface{}
	if err = c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	body["updated_at"] = time.Now().UTC()
	delete(body, "_id")
	delete(body, "id")
	delete(body, "created_at")

	res, err := h.col().UpdateOne(c.Request.Context(), bson.M{"_id": oid}, bson.M{"$set": body})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if res.MatchedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog config not found"})
		return
	}

	var updated models.BlogConfig
	if err = h.col().FindOne(c.Request.Context(), bson.M{"_id": oid}).Decode(&updated); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete godoc
// @Summary Delete a blog config
// @Tags blog
// @Param id path string true "BlogConfig ID"
// @Router /api/v1/blog/configs/{id} [delete]
func (h *BlogConfigHandler) Delete(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	res, err := h.col().DeleteOne(c.Request.Context(), bson.M{"_id": oid})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if res.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog config not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// BlogPostHandler
// ---------------------------------------------------------------------------

type BlogPostHandler struct {
	db          *mongo.Database
	asynqClient *asynq.Client
}

func NewBlogPostHandler(db *mongo.Database, asynqClient *asynq.Client) *BlogPostHandler {
	return &BlogPostHandler{db: db, asynqClient: asynqClient}
}

func (h *BlogPostHandler) col() *mongo.Collection {
	return h.db.Collection("blog_posts")
}

// TriggerInput is the request body for POST /api/v1/blog/generate.
type TriggerInput struct {
	BlogConfigID string `json:"blog_config_id" binding:"required"`
	SeedKeyword  string `json:"seed_keyword"   binding:"required"`
	Language     string `json:"language"`
	Tone         string `json:"tone"`
	WordCount    int    `json:"word_count"`
}

// Trigger godoc
// @Summary Manually trigger blog post generation
// @Tags blog
// @Accept json
// @Produce json
// @Router /api/v1/blog/generate [post]
func (h *BlogPostHandler) Trigger(c *gin.Context) {
	var input TriggerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	configOID, err := bson.ObjectIDFromHex(input.BlogConfigID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid blog_config_id"})
		return
	}

	var cfg models.BlogConfig
	if err = h.db.Collection("blog_configs").FindOne(c.Request.Context(), bson.M{"_id": configOID}).Decode(&cfg); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog config not found"})
		return
	}

	// Apply defaults from config when overrides are not provided.
	lang := input.Language
	if lang == "" {
		lang = cfg.DefaultLanguage
	}
	tone := input.Tone
	if tone == "" {
		tone = cfg.DefaultTone
	}
	wc := input.WordCount
	if wc == 0 {
		wc = cfg.DefaultWordCount
	}

	now := time.Now().UTC()
	post := models.BlogPost{
		ID:           bson.NewObjectID(),
		ProjectID:    cfg.ProjectID,
		BlogConfigID: configOID,
		SeedKeyword:  input.SeedKeyword,
		Language:     lang,
		Tone:         tone,
		WordCount:    wc,
		Status:       models.BlogPostStatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if _, err = h.col().InsertOne(c.Request.Context(), post); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create blog post: " + err.Error()})
		return
	}

	task, err := worker.NewBlogGenerateTask(worker.BlogGeneratePayload{
		BlogPostID:   post.ID.Hex(),
		BlogConfigID: configOID.Hex(),
		ProjectID:    cfg.ProjectID.Hex(),
		SeedKeyword:  input.SeedKeyword,
		Language:     lang,
		Tone:         tone,
		WordCount:    wc,
	})
	if err != nil {
		h.markFailed(c, post.ID, "failed to build task: "+err.Error())
		return
	}

	if _, err = h.asynqClient.EnqueueContext(c.Request.Context(), task); err != nil {
		h.markFailed(c, post.ID, "failed to enqueue task: "+err.Error())
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"blog_post_id": post.ID.Hex(),
		"status":       post.Status,
	})
}

// List godoc
// @Summary List blog posts with optional filters
// @Tags blog
// @Produce json
// @Param project_id query string false "Project ID"
// @Param status     query string false "Status filter"
// @Router /api/v1/blog/posts [get]
func (h *BlogPostHandler) List(c *gin.Context) {
	filter := bson.M{}
	if pid := c.Query("project_id"); pid != "" {
		oid, err := bson.ObjectIDFromHex(pid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid project_id"})
			return
		}
		filter["project_id"] = oid
	}
	if s := c.Query("status"); s != "" {
		filter["status"] = models.BlogPostStatus(s)
	}

	cursor, err := h.col().Find(c.Request.Context(), filter, options.Find().SetSort(bson.M{"created_at": -1}))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer cursor.Close(c.Request.Context()) //nolint:errcheck

	var posts []models.BlogPost
	if err = cursor.All(c.Request.Context(), &posts); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, posts)
}

// Get godoc
// @Summary Get a blog post by ID (includes full pipeline data)
// @Tags blog
// @Produce json
// @Param id path string true "BlogPost ID"
// @Router /api/v1/blog/posts/{id} [get]
func (h *BlogPostHandler) Get(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var post models.BlogPost
	if err = h.col().FindOne(c.Request.Context(), bson.M{"_id": oid}).Decode(&post); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog post not found"})
		return
	}
	c.JSON(http.StatusOK, post)
}

// Delete godoc
// @Summary Delete a blog post
// @Tags blog
// @Param id path string true "BlogPost ID"
// @Router /api/v1/blog/posts/{id} [delete]
func (h *BlogPostHandler) Delete(c *gin.Context) {
	oid, err := bson.ObjectIDFromHex(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	res, err := h.col().DeleteOne(c.Request.Context(), bson.M{"_id": oid})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if res.DeletedCount == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "blog post not found"})
		return
	}
	c.Status(http.StatusNoContent)
}

// markFailed sets a pending post to failed and responds with 500.
func (h *BlogPostHandler) markFailed(c *gin.Context, id bson.ObjectID, errMsg string) {
	_, _ = h.col().UpdateOne(c.Request.Context(), bson.M{"_id": id}, bson.M{
		"$set": bson.M{
			"status":     models.BlogPostStatusFailed,
			"error":      errMsg,
			"updated_at": time.Now().UTC(),
		},
	})
	c.JSON(http.StatusInternalServerError, gin.H{"error": errMsg})
}

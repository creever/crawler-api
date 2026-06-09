package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	dbName := os.Getenv("MONGO_DB")
	if dbName == "" {
		dbName = "crawler"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := mongo.Connect(options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx) //nolint:errcheck

	col := client.Database(dbName).Collection("users")

	// Check if an admin already exists.
	count, err := col.CountDocuments(ctx, bson.M{"role": "admin"})
	if err != nil {
		log.Fatalf("count: %v", err)
	}
	if count > 0 {
		fmt.Println("Admin user already exists — no action taken.")
		return
	}

	password := "Admin@Crawler2025!"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		log.Fatalf("bcrypt: %v", err)
	}

	now := time.Now().UTC()
	user := bson.M{
		"_id":           bson.NewObjectID(),
		"email":         "admin@crawler.local",
		"password_hash": string(hash),
		"name":          "Admin",
		"role":          "admin",
		"active":        true,
		"created_at":    now,
		"updated_at":    now,
	}

	if _, err = col.InsertOne(ctx, user); err != nil {
		log.Fatalf("insert: %v", err)
	}

	fmt.Println("✅ Admin user seeded successfully.")
	fmt.Println("   Email:    admin@crawler.local")
	fmt.Println("   Password: Admin@Crawler2025!")
	fmt.Println("   Role:     admin")
	fmt.Println()
	fmt.Println("⚠️  Change this password after first login via PATCH /api/v1/users/:id/password")
}

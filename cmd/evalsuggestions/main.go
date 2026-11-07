// Command evalsuggestions prints a Markdown report to compare stored legacy
// suggestions with new page analyses on real opportunities. Nothing is stored
// except the page snapshot cache. It calls the configured LLM once or twice
// per page.
//
// Usage:
//
//	evalsuggestions -project <id> [-pages 12]
//
// MONGO_URI, MONGO_DB and CRAWLER_ADDR come from the environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/config"
	"github.com/creever/crawler-api/db"
	"github.com/creever/crawler-api/models"
	"github.com/creever/crawler-api/worker"
)

func main() {
	projectID := flag.String("project", "", "project ID")
	pages := flag.Int("pages", 12, "number of pages, spread across signal types")
	flag.Parse()

	oid, err := bson.ObjectIDFromHex(*projectID)
	if err != nil {
		flag.Usage()
		os.Exit(2)
	}

	cfg := config.Load()
	client := db.Connect(cfg.MongoURI)
	defer db.Disconnect(client)
	database := client.Database(cfg.MongoDB)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	var project models.Project
	if err = database.Collection("projects").FindOne(ctx, bson.M{"_id": oid}).Decode(&project); err != nil {
		log.Fatalf("project: %v", err)
	}
	proc := worker.NewProcessor(database, zap.NewNop(), cfg.CrawlerAddr, nil)

	targets, err := pickTargets(ctx, database, oid, *pages)
	if err != nil {
		log.Fatalf("opportunities: %v", err)
	}
	fmt.Printf("# Javaslat-értékelés: %s\n\nOldalanként pontozd 0-2 skálán: helyes-e a diagnózis, megalapozott-e, végrehajtanád-e.\n", project.Name)
	for page, opps := range targets {
		fmt.Printf("\n---\n\n## %s\n\nJelek: %s\n", page, signals(opps))
		printLegacy(ctx, database, oid, page)
		a, suggestions, aErr := proc.PreviewAnalysis(ctx, &project, page, opps)
		if aErr != nil {
			fmt.Printf("\n### Új rendszer\n\nHIBA: %v\n", aErr)
			continue
		}
		printAnalysis(a, suggestions)
	}
}

// pickTargets takes the top opportunities round-robin across signal types, so
// every type is represented.
func pickTargets(ctx context.Context, database *mongo.Database, projectID bson.ObjectID, pages int) (map[string][]models.Opportunity, error) {
	cursor, err := database.Collection("opportunities").Find(ctx,
		bson.M{"project_id": projectID, "page": bson.M{"$ne": ""}},
		options.Find().SetSort(bson.D{{Key: "rank", Value: 1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var opps []models.Opportunity
	if err = cursor.All(ctx, &opps); err != nil {
		return nil, err
	}

	byType := make(map[models.OpportunityType][]models.Opportunity)
	var order []models.OpportunityType
	byPage := make(map[string][]models.Opportunity)
	for _, o := range opps {
		if _, ok := byType[o.Type]; !ok {
			order = append(order, o.Type)
		}
		byType[o.Type] = append(byType[o.Type], o)
		byPage[o.Page] = append(byPage[o.Page], o)
	}

	picked := make(map[string][]models.Opportunity)
	for progress := true; progress && len(picked) < pages; {
		progress = false
		for _, t := range order {
			for len(byType[t]) > 0 && len(picked) < pages {
				o := byType[t][0]
				byType[t] = byType[t][1:]
				if _, dup := picked[o.Page]; dup {
					continue
				}
				picked[o.Page] = byPage[o.Page]
				progress = true
				break
			}
		}
	}
	return picked, nil
}

func signals(opps []models.Opportunity) string {
	parts := make([]string, 0, len(opps))
	for _, o := range opps {
		parts = append(parts, fmt.Sprintf("%s (%s)", o.Type, o.Query))
	}
	return strings.Join(parts, ", ")
}

func printLegacy(ctx context.Context, database *mongo.Database, projectID bson.ObjectID, page string) {
	fmt.Print("\n### Régi rendszer (utoljára mentett javaslatok)\n\n")
	cursor, err := database.Collection("suggestions").Find(ctx,
		bson.M{"project_id": projectID, "page": page, "analysis_id": bson.M{"$exists": false}},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(3),
	)
	if err != nil {
		fmt.Printf("HIBA: %v\n", err)
		return
	}
	defer cursor.Close(ctx) //nolint:errcheck
	var old []models.Suggestion
	if err = cursor.All(ctx, &old); err != nil || len(old) == 0 {
		fmt.Println("Nincs mentett régi javaslat ehhez az oldalhoz.")
		return
	}
	for _, s := range old {
		fmt.Printf("- **%s:** %s\n  - Indoklás: %s\n", s.Kind, s.ProposedValue, s.Reason)
	}
}

func printAnalysis(a *models.PageAnalysis, suggestions []models.Suggestion) {
	fmt.Printf("\n### Új rendszer: %s", a.Status)
	if a.Confidence != "" {
		fmt.Printf(" (megbízhatóság: %s)", a.Confidence)
	}
	fmt.Println()
	if a.StatusReason != "" {
		fmt.Printf("\n%s\n", a.StatusReason)
	}
	if a.PrimaryIssue != "" {
		fmt.Printf("\n**Probléma:** %s\n\n**Gyökérok:** %s\n", a.PrimaryIssue, a.RootCause)
	}
	if len(a.Evidence) > 0 {
		fmt.Print("\n**Bizonyítékok:**\n")
		for _, e := range a.Evidence {
			fmt.Printf("- [%s] %s\n  - Forrás: %s\n", e.Ref, e.Claim, e.Fact)
		}
	}
	if a.NoActionReason != "" {
		fmt.Printf("\n**Nincs teendő:** %s\n", a.NoActionReason)
	}
	if len(suggestions) > 0 {
		fmt.Print("\n**Akciók:**\n")
		for _, s := range suggestions {
			fmt.Printf("%d. **%s** (%s): %s\n   - Indoklás: %s\n   - Hivatkozás: %s\n",
				s.Priority, s.Kind, s.Effort, s.ProposedValue, s.Reason, strings.Join(s.EvidenceRefs, ", "))
		}
	}
	if len(a.ValidationWarnings) > 0 {
		fmt.Printf("\n**Validátor:** %s\n", strings.Join(a.ValidationWarnings, "; "))
	}
	fmt.Printf("\nLátott oldalak: %d, elküldött karakter: %d, lekérdezések: %d, GA4: %v, csonkolt: %s\n",
		len(a.Coverage.PagesRead), a.Coverage.CharsSent, a.Coverage.GSCQueries, a.Coverage.HasGA4, strings.Join(a.Coverage.Truncated, ", "))
}

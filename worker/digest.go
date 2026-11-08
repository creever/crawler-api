package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime"
	"net"
	"net/smtp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"

	"github.com/creever/crawler-api/models"
)

const (
	digestWeekday = time.Monday
	digestHourUTC = 7
	digestEvery   = 6 * 24 * time.Hour
	digestWeek    = 7 * 24 * time.Hour
)

// ErrEmailNotConfigured means the SMTP settings are missing.
var ErrEmailNotConfigured = errors.New("email is not configured: set smtp_host and smtp_from in Settings")

// DigestIssue is an audit issue that grew during the week.
type DigestIssue struct {
	Code  string
	Pages int
	Added int
}

// DigestData is one project's week.
type DigestData struct {
	Project          string
	HasAudit         bool
	Score            float64
	PrevScore        float64
	HasPrev          bool
	Critical         int
	Warning          int
	Growing          []DigestIssue
	Clicks           float64
	PrevClicks       float64
	Impressions      float64
	LastSearchDate   string
	OpenTasks        int64
	ToReview         int64
	DoneThisWeek     int64
	Improved         int64
	NoChange         int64
	Worse            int64
	TopOpportunities []models.Opportunity
	BotRequests      int64
	AIRequests       int64
	BotErrors        int64
}

// Digest is a rendered summary.
type Digest struct {
	Subject string
	Text    string
	HTML    string
}

func audits(ctx context.Context, db *mongo.Database, id bson.ObjectID, now time.Time) (latest, prev *models.SiteAudit, err error) {
	cursor, err := db.Collection("site_audits").Find(ctx, bson.M{"project_id": id, "status": "done", "pages_fetched": bson.M{"$gt": 0}},
		options.Find().SetSort(bson.D{{Key: "started_at", Value: -1}}).SetLimit(30).SetProjection(bson.M{"groups": 0}))
	if err != nil {
		return nil, nil, err
	}
	var rows []models.SiteAudit
	if err = cursor.All(ctx, &rows); err != nil || len(rows) == 0 {
		return nil, nil, err
	}
	latest = &rows[0]
	for i := 1; i < len(rows); i++ {
		prev = &rows[i]
		if !rows[i].StartedAt.After(now.Add(-digestWeek)) {
			break
		}
	}
	return latest, prev, nil
}

// growingIssues lists the issues on more pages than a week ago, worst first.
func growingIssues(now, before map[string]int) []DigestIssue {
	var out []DigestIssue
	for code, n := range now {
		if n > before[code] {
			out = append(out, DigestIssue{Code: code, Pages: n, Added: n - before[code]})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Added != out[j].Added {
			return out[i].Added > out[j].Added
		}
		return out[i].Code < out[j].Code
	})
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

// CollectDigest gathers a project's week.
func CollectDigest(ctx context.Context, db *mongo.Database, project *models.Project, now time.Time) (*DigestData, error) {
	d := &DigestData{Project: project.Name}
	latest, prev, err := audits(ctx, db, project.ID, now)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		d.HasAudit, d.Score = true, latest.Score
		d.Critical, d.Warning = latest.IssuesBySeverity[models.SeverityCritical], latest.IssuesBySeverity[models.SeverityWarning]
		if prev != nil {
			d.HasPrev, d.PrevScore = true, prev.Score
			d.Growing = growingIssues(latest.IssuesByCode, prev.IssuesByCode)
		}
	}

	var last struct {
		Date string `bson:"date"`
	}
	err = db.Collection("gsc_rows").FindOne(ctx, bson.M{"project_id": project.ID},
		options.FindOne().SetSort(bson.D{{Key: "date", Value: -1}}).SetProjection(bson.M{"date": 1})).Decode(&last)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, err
	}
	if end, parseErr := time.Parse("2006-01-02", last.Date); err == nil && parseErr == nil {
		d.LastSearchDate = last.Date
		curStart := end.AddDate(0, 0, -6).Format("2006-01-02")
		prevStart := end.AddDate(0, 0, -13).Format("2006-01-02")
		cursor, aggErr := db.Collection("gsc_rows").Aggregate(ctx, mongo.Pipeline{
			{{Key: "$match", Value: bson.M{"project_id": project.ID, "date": bson.M{"$gte": prevStart, "$lte": last.Date}}}},
			{{Key: "$group", Value: bson.M{"_id": bson.M{"$gte": bson.A{"$date", curStart}}, "clicks": bson.M{"$sum": "$clicks"}, "impressions": bson.M{"$sum": "$impressions"}}}},
		})
		if aggErr != nil {
			return nil, aggErr
		}
		var rows []struct {
			Current     bool    `bson:"_id"`
			Clicks      float64 `bson:"clicks"`
			Impressions float64 `bson:"impressions"`
		}
		if aggErr = cursor.All(ctx, &rows); aggErr != nil {
			return nil, aggErr
		}
		for _, r := range rows {
			if r.Current {
				d.Clicks, d.Impressions = r.Clicks, r.Impressions
			} else {
				d.PrevClicks = r.Clicks
			}
		}
	}

	weekAgo := now.Add(-digestWeek)
	tasks := db.Collection("seo_tasks")
	count := func(col *mongo.Collection, f bson.M) (int64, error) { return col.CountDocuments(ctx, f) }
	open := []models.TaskStatus{models.TaskStatusOpen, models.TaskStatusQueued, models.TaskStatusClaimed, models.TaskStatusReopened}
	if d.OpenTasks, err = count(tasks, bson.M{"project_id": project.ID, "status": bson.M{"$in": open}}); err != nil {
		return nil, err
	}
	proposed, err := count(tasks, bson.M{"project_id": project.ID, "status": models.TaskStatusProposed})
	if err != nil {
		return nil, err
	}
	pending, err := count(db.Collection("suggestions"), bson.M{"project_id": project.ID, "status": models.SuggestionStatusPending})
	if err != nil {
		return nil, err
	}
	d.ToReview = proposed + pending
	if d.DoneThisWeek, err = count(tasks, bson.M{"project_id": project.ID, "done_at": bson.M{"$gte": weekAgo}}); err != nil {
		return nil, err
	}
	for verdict, into := range map[string]*int64{models.VerdictImproved: &d.Improved, models.VerdictNoChange: &d.NoChange, models.VerdictWorse: &d.Worse} {
		if *into, err = count(tasks, bson.M{"project_id": project.ID, "measurement.verdict": verdict, "measurement.updated_at": bson.M{"$gte": weekAgo}}); err != nil {
			return nil, err
		}
	}
	oc, err := db.Collection("opportunities").Find(ctx, bson.M{"project_id": project.ID}, options.Find().SetSort(bson.D{{Key: "rank", Value: 1}}).SetLimit(3))
	if err != nil {
		return nil, err
	}
	if err = oc.All(ctx, &d.TopOpportunities); err != nil {
		return nil, err
	}
	logs := db.Collection("prerender_data")
	base := bson.M{"project_id": project.ID, "cached_at": bson.M{"$gte": weekAgo}}
	with := func(k string, v interface{}) bson.M {
		m := bson.M{k: v}
		for a, b := range base {
			m[a] = b
		}
		return m
	}
	if d.BotRequests, err = count(logs, with("requester_type", bson.M{"$ne": models.RequesterTypeHuman})); err != nil {
		return nil, err
	}
	if d.AIRequests, err = count(logs, with("requester_type", models.RequesterTypeAI)); err != nil {
		return nil, err
	}
	if d.BotErrors, err = count(logs, with("status_code", bson.M{"$gte": 400})); err != nil {
		return nil, err
	}
	return d, nil
}

func thousands(n float64) string {
	s := strconv.FormatInt(int64(n+0.5), 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func changeText(cur, prev float64) string {
	if prev == 0 {
		return ""
	}
	return fmt.Sprintf(" (%+.0f%% on the week before)", (cur-prev)/prev*100)
}

// RenderDigest writes the summary as plain text and HTML. Each line is one
// fact; a section with nothing to say is left out.
func RenderDigest(d *DigestData) *Digest {
	type section struct {
		title string
		lines []string
	}
	var sections []section
	if d.HasAudit {
		line := fmt.Sprintf("Score %.0f%%", d.Score*100)
		if d.HasPrev {
			line += fmt.Sprintf(", %+.0f points on the week before", (d.Score-d.PrevScore)*100)
		}
		lines := []string{line, fmt.Sprintf("%d pages with critical issues, %d with warnings", d.Critical, d.Warning)}
		for _, g := range d.Growing {
			lines = append(lines, fmt.Sprintf("Growing: %s on %d pages (+%d)", strings.ReplaceAll(g.Code, "_", " "), g.Pages, g.Added))
		}
		sections = append(sections, section{"Site health", lines})
	}
	if d.LastSearchDate != "" {
		sections = append(sections, section{"Search, last 7 days of data", []string{
			fmt.Sprintf("%s clicks%s", thousands(d.Clicks), changeText(d.Clicks, d.PrevClicks)),
			fmt.Sprintf("%s impressions, data up to %s", thousands(d.Impressions), d.LastSearchDate),
		}})
	}
	work := []string{fmt.Sprintf("%d open tasks, %d waiting for review, %d finished this week", d.OpenTasks, d.ToReview, d.DoneThisWeek)}
	if m := d.Improved + d.NoChange + d.Worse; m > 0 {
		work = append(work, fmt.Sprintf("Measured results: %d improved, %d no change, %d worse", d.Improved, d.NoChange, d.Worse))
	}
	sections = append(sections, section{"Work", work})
	if len(d.TopOpportunities) > 0 {
		var lines []string
		for _, o := range d.TopOpportunities {
			what := o.Query
			if what == "" {
				what = o.Page
			}
			lines = append(lines, fmt.Sprintf("%s: %s, about +%s clicks in 28 days", strings.ReplaceAll(string(o.Type), "_", " "), what, thousands(o.Score)))
		}
		sections = append(sections, section{"Biggest opportunities", lines})
	}
	if d.BotRequests > 0 {
		sections = append(sections, section{"Bots, last 7 days", []string{
			fmt.Sprintf("%s prerender requests, %s from AI crawlers, %s answered with an error page", thousands(float64(d.BotRequests)), thousands(float64(d.AIRequests)), thousands(float64(d.BotErrors))),
		}})
	}

	subject := "Weekly SEO summary: " + d.Project
	var text, body strings.Builder
	fmt.Fprintf(&text, "%s\n", subject)
	fmt.Fprintf(&body, `<div style="font-family:Arial,sans-serif;color:#0f172a;max-width:560px"><h1 style="font-size:20px">%s</h1>`, html.EscapeString(subject))
	for _, s := range sections {
		fmt.Fprintf(&text, "\n%s\n", s.title)
		fmt.Fprintf(&body, `<h2 style="font-size:15px;margin:18px 0 6px">%s</h2><ul style="margin:0;padding-left:18px;font-size:14px;line-height:1.5">`, html.EscapeString(s.title))
		for _, l := range s.lines {
			fmt.Fprintf(&text, "- %s\n", l)
			fmt.Fprintf(&body, "<li>%s</li>", html.EscapeString(l))
		}
		body.WriteString("</ul>")
	}
	body.WriteString("</div>")
	return &Digest{Subject: subject, Text: text.String(), HTML: body.String()}
}

// smtpConfig is read from the global settings.
type smtpConfig struct {
	Host, Port, Username, Password, From string
}

func loadSMTP(ctx context.Context, db *mongo.Database) (*smtpConfig, error) {
	c := &smtpConfig{
		Host: settingValue(ctx, db, "smtp_host"), Port: settingValue(ctx, db, "smtp_port"),
		Username: settingValue(ctx, db, "smtp_username"), Password: settingValue(ctx, db, "smtp_password"),
		From: settingValue(ctx, db, "smtp_from"),
	}
	if c.Host == "" || c.From == "" {
		return nil, ErrEmailNotConfigured
	}
	if c.Port == "" {
		c.Port = "587"
	}
	return c, nil
}

// EmailConfigured reports whether email can be sent.
func EmailConfigured(ctx context.Context, db *mongo.Database) bool {
	_, err := loadSMTP(ctx, db)
	return err == nil
}

// DigestRecipient returns the project owner's address, or "".
func DigestRecipient(ctx context.Context, db *mongo.Database, project *models.Project) string {
	var owner models.User
	if err := db.Collection("users").FindOne(ctx, bson.M{"_id": project.OwnerID}).Decode(&owner); err != nil {
		return ""
	}
	return owner.Email
}

// LastDigestAt returns when the project's summary last went out, or nil.
func LastDigestAt(ctx context.Context, db *mongo.Database, projectID bson.ObjectID) *time.Time {
	var row struct {
		SentAt time.Time `bson:"sent_at"`
	}
	if err := db.Collection("digest_log").FindOne(ctx, bson.M{"project_id": projectID},
		options.FindOne().SetSort(bson.D{{Key: "sent_at", Value: -1}})).Decode(&row); err != nil {
		return nil
	}
	return &row.SentAt
}

func headerSafe(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// digestMessage builds the MIME message with text and HTML parts.
func digestMessage(from, to string, d *Digest) []byte {
	const boundary = "crawler-digest-boundary"
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n", headerSafe(from), headerSafe(to), mime.QEncoding.Encode("utf-8", headerSafe(d.Subject)))
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", boundary, strings.ReplaceAll(d.Text, "\n", "\r\n"))
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\n\r\n%s\r\n", boundary, d.HTML)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return []byte(b.String())
}

// sendMail is a variable so that tests need no mail server.
var sendMail = func(c *smtpConfig, to string, msg []byte) error {
	addr := net.JoinHostPort(c.Host, c.Port)
	var client *smtp.Client
	var err error
	if c.Port == "465" {
		conn, dialErr := tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12})
		if dialErr != nil {
			return dialErr
		}
		client, err = smtp.NewClient(conn, c.Host)
	} else {
		client, err = smtp.Dial(addr)
	}
	if err != nil {
		return err
	}
	defer client.Close() //nolint:errcheck
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err = client.StartTLS(&tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if c.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err = client.Mail(c.From); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(msg); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// SendDigest sends the project's summary to its owner and records it.
func SendDigest(ctx context.Context, db *mongo.Database, project *models.Project, now time.Time) error {
	cfg, err := loadSMTP(ctx, db)
	if err != nil {
		return err
	}
	to := DigestRecipient(ctx, db, project)
	if to == "" {
		return fmt.Errorf("the project has no owner with an email address")
	}
	data, err := CollectDigest(ctx, db, project, now)
	if err != nil {
		return err
	}
	d := RenderDigest(data)
	if err = sendMail(cfg, to, digestMessage(cfg.From, to, d)); err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	_, err = db.Collection("digest_log").InsertOne(ctx, bson.M{"project_id": project.ID, "sent_at": now, "to": to, "subject": d.Subject})
	return err
}

// DigestPayload names the project of a digest:send task.
type DigestPayload struct {
	ProjectID string `json:"project_id"`
}

// HandleDigestSend sends one project's weekly summary.
func (p *Processor) HandleDigestSend(ctx context.Context, t *asynq.Task) error {
	var payload DigestPayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: unmarshal digest:send payload: %v", asynq.SkipRetry, err)
	}
	id, err := bson.ObjectIDFromHex(payload.ProjectID)
	if err != nil {
		return fmt.Errorf("%w: invalid project_id: %v", asynq.SkipRetry, err)
	}
	var project models.Project
	if err = p.db.Collection("projects").FindOne(ctx, bson.M{"_id": id}).Decode(&project); err != nil {
		return fmt.Errorf("%w: project not found: %v", asynq.SkipRetry, err)
	}
	err = SendDigest(ctx, p.db, &project, time.Now().UTC())
	if errors.Is(err, ErrEmailNotConfigured) {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	return err
}

// digestDue reports whether this hour is when summaries go out.
func digestDue(now time.Time) bool {
	return now.Weekday() == digestWeekday && now.Hour() >= digestHourUTC
}

// StartDigestScheduler sends the weekly summaries of the projects that asked.
func StartDigestScheduler(db *mongo.Database, asynqClient *asynq.Client, logger *zap.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	go func() {
		for range ticker.C {
			now := time.Now().UTC()
			if digestDue(now) {
				enqueueDigests(db, asynqClient, logger, now)
			}
		}
	}()
	logger.Info("digest scheduler started")
}

func enqueueDigests(db *mongo.Database, client *asynq.Client, logger *zap.Logger, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := db.Collection("projects").Find(ctx, bson.M{"active": true, "weekly_digest": true}, options.Find().SetProjection(bson.M{"_id": 1}))
	if err != nil {
		logger.Warn("digest: projects not read", zap.Error(err))
		return
	}
	var projects []models.Project
	if err = cursor.All(ctx, &projects); err != nil {
		return
	}
	for _, pr := range projects {
		if last := LastDigestAt(ctx, db, pr.ID); last != nil && now.Sub(*last) < digestEvery {
			continue
		}
		payload, _ := json.Marshal(DigestPayload{ProjectID: pr.ID.Hex()})
		if _, err = client.EnqueueContext(ctx, asynq.NewTask(TypeDigestSend, payload, asynq.MaxRetry(2), asynq.Unique(12*time.Hour))); err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
			logger.Warn("digest not queued", zap.String("project_id", pr.ID.Hex()), zap.Error(err))
		}
	}
}

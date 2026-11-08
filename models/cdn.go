package models

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// CDNProviderBunny is Bunny.net.
const CDNProviderBunny = "bunny"

// CDNPurge records one purge. MongoDB collection: cdn_purges (kept 30 days)
type CDNPurge struct {
	ID        bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID bson.ObjectID `bson:"project_id"    json:"project_id"`
	At        time.Time     `bson:"at"            json:"at"`
	Trigger   string        `bson:"trigger"       json:"trigger"` // task | manual
	All       bool          `bson:"all"           json:"all"`     // the whole zone
	URLs      int           `bson:"urls"          json:"urls"`
	Failed    int           `bson:"failed"        json:"failed"`
	Skipped   int           `bson:"skipped"       json:"skipped"` // over the cap of one purge
	Error     string        `bson:"error,omitempty" json:"error,omitempty"`
}

// CoreVitals are Core Web Vitals; a zero value means no data.
type CoreVitals struct {
	LCPMs float64 `bson:"lcp_ms" json:"lcp_ms"`
	INPMs float64 `bson:"inp_ms" json:"inp_ms"`
	CLS   float64 `bson:"cls"    json:"cls"`
}

// PageVitals is one page's speed in one run, for mobile.
// MongoDB collection: page_vitals (kept 180 days)
type PageVitals struct {
	ID          bson.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	ProjectID   bson.ObjectID `bson:"project_id"    json:"project_id"`
	RunAt       time.Time     `bson:"run_at"        json:"run_at"`
	URL         string        `bson:"url"           json:"url"`
	Group       string        `bson:"group"         json:"group"`
	Impressions float64       `bson:"impressions"   json:"impressions"`
	// Field is what real Chrome users experienced; nil without enough data.
	Field *CoreVitals `bson:"field,omitempty" json:"field,omitempty"`
	// FieldOverall is FAST, AVERAGE or SLOW.
	FieldOverall string `bson:"field_overall,omitempty" json:"field_overall,omitempty"`
	// OriginFallback: the page had too little data; Field is the whole site's.
	OriginFallback bool `bson:"origin_fallback" json:"origin_fallback"`
	// LabScore is Lighthouse's performance score, 0..1, from one simulated load.
	LabScore float64 `bson:"lab_score"      json:"lab_score"`
	LabLCPMs float64 `bson:"lab_lcp_ms"     json:"lab_lcp_ms"`
	LabTBTMs float64 `bson:"lab_tbt_ms"     json:"lab_tbt_ms"`
	LabCLS   float64 `bson:"lab_cls"        json:"lab_cls"`
	Error    string  `bson:"error,omitempty" json:"error,omitempty"`
}

package models

import "time"

// SEOResultPayload is the structured result returned by the crawler after
// performing an SEO analysis on a page. It is used as the task result for
// crawl:seo jobs and is persisted in the seo_data collection.
type SEOResultPayload struct {
	ProjectID string    `json:"project_id"`
	CrawledAt time.Time `json:"crawled_at"`

	// Core
	URL             string `json:"url"`
	Title           string `json:"title"`
	MetaDescription string `json:"meta_description"`
	MetaKeywords    string `json:"meta_keywords"`
	Canonical       string `json:"canonical"`
	Robots          string `json:"robots"`

	// Open Graph
	OGTitle       string `json:"og_title"`
	OGDescription string `json:"og_description"`
	OGImage       string `json:"og_image"`
	OGURL         string `json:"og_url"`

	// Headings
	H1 []string `json:"h1"`
	H2 []string `json:"h2"`
	H3 []string `json:"h3"`
	H4 []string `json:"h4"`
	H5 []string `json:"h5"`
	H6 []string `json:"h6"`

	// Links
	InternalLinks []string `json:"internal_links"`
	ExternalLinks []string `json:"external_links"`

	// Images
	TotalImages      int `json:"total_images"`
	ImagesWithoutAlt int `json:"images_without_alt"`

	// Content
	WordCount int `json:"word_count"`
}

// RenderPayload is sent to POST /projects/{project_id}/renders after a page is
// pre-rendered by chromedp. The API stores the HTML so it can be served as a
// pre-render proxy for JavaScript-based URLs.
type RenderPayload struct {
	ProjectID string    `json:"project_id"`
	CrawledAt time.Time `json:"crawled_at"`
	URL       string    `json:"url"`
	HTML      string    `json:"html"`
}

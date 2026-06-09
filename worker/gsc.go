package worker

// Google Search Console integration.
// Authentication uses the service account JWT Bearer grant — no oauth2 library
// needed; we reuse github.com/golang-jwt/jwt/v5 which is already a direct dep.

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// gscQuery is a single row from the GSC Search Analytics API.
type gscQuery struct {
	Query       string
	Clicks      int
	Impressions int
	CTR         float64 // 0–1
	Position    float64
}

type serviceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// fetchGSCKeywords authenticates with Google Search Console via the service
// account credentials and returns the top queries for propertyURL sorted by
// impressions (last 90 days).
func (p *Processor) fetchGSCKeywords(ctx context.Context, credentials, propertyURL string) ([]gscQuery, error) {
	var sa serviceAccountKey
	if err := json.Unmarshal([]byte(credentials), &sa); err != nil {
		return nil, fmt.Errorf("parse service account credentials: %w", err)
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, fmt.Errorf("service account missing client_email or private_key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}

	accessToken, err := p.gscGetAccessToken(ctx, sa)
	if err != nil {
		return nil, fmt.Errorf("get GSC access token: %w", err)
	}
	return p.gscQueryAnalytics(ctx, accessToken, propertyURL)
}

// gscGetAccessToken exchanges a signed JWT for a Google OAuth2 access token
// using the JWT Bearer grant (RFC 7523).
func (p *Processor) gscGetAccessToken(ctx context.Context, sa serviceAccountKey) (string, error) {
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block from private key")
	}
	rawKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	privateKey, ok := rawKey.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("private key is not RSA")
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   sa.ClientEmail,
		"sub":   sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/webmasters.readonly",
		"aud":   sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(privateKey)
	if err != nil {
		return "", fmt.Errorf("sign JWT: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signed},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sa.TokenURI,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tokenResp.Error != "" {
		return "", fmt.Errorf("token error %s: %s", tokenResp.Error, tokenResp.ErrorDesc)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("empty access token in response")
	}
	return tokenResp.AccessToken, nil
}

// gscQueryAnalytics fetches the top 100 search queries for the GSC property
// over the last 90 days, ordered by impressions descending.
func (p *Processor) gscQueryAnalytics(ctx context.Context, accessToken, propertyURL string) ([]gscQuery, error) {
	// GSC data has a ~3-day processing lag.
	end := time.Now().UTC().AddDate(0, 0, -3)
	start := end.AddDate(0, -3, 0)

	reqBody, _ := json.Marshal(map[string]interface{}{
		"startDate":  start.Format("2006-01-02"),
		"endDate":    end.Format("2006-01-02"),
		"dimensions": []string{"query"},
		"rowLimit":   100,
		"orderBy": []map[string]string{
			{"fieldName": "impressions", "sortOrder": "DESCENDING"},
		},
	})

	apiURL := fmt.Sprintf(
		"https://searchconsole.googleapis.com/webmasters/v3/sites/%s/searchAnalytics/query",
		url.PathEscape(propertyURL),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build GSC analytics request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GSC analytics request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GSC API HTTP %s: %s", resp.Status, truncate(string(b), 300))
	}

	var result struct {
		Rows []struct {
			Keys        []string `json:"keys"`
			Clicks      float64  `json:"clicks"`
			Impressions float64  `json:"impressions"`
			CTR         float64  `json:"ctr"`
			Position    float64  `json:"position"`
		} `json:"rows"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode GSC response: %w", err)
	}

	queries := make([]gscQuery, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row.Keys) == 0 {
			continue
		}
		queries = append(queries, gscQuery{
			Query:       row.Keys[0],
			Clicks:      int(row.Clicks),
			Impressions: int(row.Impressions),
			CTR:         row.CTR,
			Position:    row.Position,
		})
	}
	p.logger.Info("GSC analytics fetched",
		zap.Int("queries", len(queries)),
		zap.String("property", propertyURL),
	)
	return queries, nil
}

// formatGSCContext converts GSC query rows into a compact text block for
// inclusion in LLM prompts. It splits queries into two groups:
// "opportunities" (position 4–20) and "top performers" (position 1–3).
func formatGSCContext(queries []gscQuery) string {
	if len(queries) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("=== GOOGLE SEARCH CONSOLE DATA (last 90 days) ===\n")

	sb.WriteString("\nCONTENT OPPORTUNITIES — ranking position 4–20 (improve to reach top 3):\n")
	written := 0
	for _, q := range queries {
		if q.Position >= 4 && q.Position <= 20 && q.Impressions >= 30 {
			sb.WriteString(fmt.Sprintf("  • %s | %d impressions | %d clicks | pos %.1f | %.1f%% CTR\n",
				q.Query, q.Impressions, q.Clicks, q.Position, q.CTR*100))
			written++
			if written >= 25 {
				break
			}
		}
	}
	if written == 0 {
		sb.WriteString("  (none)\n")
	}

	sb.WriteString("\nTOP PERFORMERS — already ranking position 1–3:\n")
	written = 0
	for _, q := range queries {
		if q.Position < 4 {
			sb.WriteString(fmt.Sprintf("  • %s | %d impressions | %d clicks | pos %.1f\n",
				q.Query, q.Impressions, q.Clicks, q.Position))
			written++
			if written >= 10 {
				break
			}
		}
	}
	if written == 0 {
		sb.WriteString("  (none)\n")
	}

	return sb.String()
}

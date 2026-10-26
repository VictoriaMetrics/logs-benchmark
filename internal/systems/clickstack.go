package systems

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// clickstack benchmarks the ClickStack (HyperDX-based observability stack).
// It has 2 different endpoints for ingestion and queries,
// addr holds whichever endpoint is relevant for the current benchmark phase.
type clickstack struct {
	addr string

	// querier-only: cached HyperDX access key (Bearer) + the logs source id,
	// bootstrapped once via register-or-login.
	mu        sync.Mutex
	accessKey string
	sourceID  string
}

// HyperDX seeds no default user; the querier registers the first user itself
// (single-shot per team) and falls back to login if the team already exists. The
// password must satisfy HyperDX's rule: 12-72 chars with lower+upper+digit+special.
const (
	clickstackEmail    = "logs-bench@example.com"
	clickstackPassword = "LogsBenchmark1!"
)

func (s *clickstack) WaitReady() error {
	return waitHTTP(s.addr, func(*http.Response, []byte) error { return nil })
}

func (s *clickstack) PrepareBatch(entries []LogEntry) (Batch, error) {
	return otlpBatch(entries)
}

func (s *clickstack) Send(ctx context.Context, b Batch) (int, error) {
	_, _, retries, err := doReq(ctx, "POST", s.addr+"/v1/logs", otlpContentType, b.Body)
	return retries, err
}

// Rows come from POST /api/v2/search, aggregates from POST /api/v2/charts/series;
// both carry a Bearer access key. Structured filters (severity, service) use SQL;
// body-text queries use a bare Lucene term so HyperDX resolves them through the
// otel_logs text index instead of a full scan.
func (s *clickstack) Queries(p QueryParams) []Query {
	app := strings.ReplaceAll(p.App, "'", "''")
	return []Query{
		{
			// Exploratory: newest 100 error lines.
			Name:   "recent_errors",
			Window: 5 * time.Minute,
			Do:     s.search(`SeverityText ILIKE 'error'`, "sql", "Timestamp,Body", "Timestamp DESC", 100),
			Check:  csSearchCheck(0),
		},
		{
			// Analytical: error count grouped by app.
			Name:   "errors_by_app",
			Window: time.Hour,
			Do:     s.series(`SeverityText ILIKE 'error'`, "sql", []string{"ServiceName"}, "1h"),
			Check:  csSeriesCheck(1),
		},
		{
			// Scoped: one service's error count.
			Name:   "app_errors",
			Window: time.Hour,
			Do:     s.series(fmt.Sprintf(`ServiceName = '%s' AND SeverityText ILIKE 'error'`, app), "sql", nil, "1h"),
			Check:  csSeriesCheck(1),
		},
		{
			// Needle: a zero-hit token
			Name:   "needle",
			Window: 24 * time.Hour,
			Do:     s.search(`7f3c9a2e-bench-needle`, "lucene", "Timestamp,Body", "", 10),
			Check:  csSearchCheck(0),
		},
		{
			// Common token: a frequent word
			Name:   "common_token",
			Window: time.Hour,
			Do:     s.search(`program`, "lucene", "Timestamp,Body", "Timestamp DESC", 100),
			Check:  csSearchCheck(1),
		},
		{
			// Time bucketing: per-minute counts.
			Name:   "time_buckets",
			Window: time.Hour,
			Do:     s.series("", "sql", nil, "1m"),
			Check:  csSeriesCheck(1),
		},
	}
}

// StoredRows counts all stored logs with a no-groupBy count series over storedRowsWindow,
// summed across its time buckets.
func (s *clickstack) StoredRows(ctx context.Context) (*int64, error) {
	end := time.Now()
	start := end.Add(-storedRowsWindow)
	do := s.series("", "sql", nil, "1h")
	body, err := do(ctx, start, end)
	if err != nil {
		return nil, err
	}
	return clickstackStoredRows(body)
}

// clickstackStoredRows sums the count across the series' time buckets. Each point
// carries the single series' aggregate under "series_0.data" (a quoted number);
// an absent field means an empty window, not an error.
func clickstackStoredRows(body []byte) (*int64, error) {
	var r struct {
		Data []struct {
			Series0Data json.RawMessage `json:"series_0.data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse clickstack stored-rows: %w: %s", err, truncate(string(body), 200))
	}
	var total int64
	for _, pt := range r.Data {
		if len(pt.Series0Data) == 0 {
			continue
		}
		n, err := jsonInt(pt.Series0Data)
		if err != nil {
			return nil, fmt.Errorf("clickstack stored rows: bad count %s: %w", truncate(string(pt.Series0Data), 40), err)
		}
		total += n
	}
	return &total, nil
}

// csSearchReq is the POST /api/v2/search body. Times are ISO 8601 strings.
type csSearchReq struct {
	SourceID      string `json:"sourceId"`
	StartTime     string `json:"startTime"`
	EndTime       string `json:"endTime"`
	Where         string `json:"where"`
	WhereLanguage string `json:"whereLanguage"` // lucene | sql
	Select        string `json:"select,omitempty"`
	OrderBy       string `json:"orderBy,omitempty"`
	MaxResults    int    `json:"maxResults"`
}

// csSeriesReq is the POST /api/v2/charts/series body. Times are epoch milliseconds.
type csSeriesReq struct {
	Series      []csSeries `json:"series"`
	StartTime   int64      `json:"startTime"`
	EndTime     int64      `json:"endTime"`
	Granularity string     `json:"granularity,omitempty"`
}

type csSeries struct {
	SourceID      string   `json:"sourceId"`
	AggFn         string   `json:"aggFn"`
	Where         string   `json:"where"`
	WhereLanguage string   `json:"whereLanguage"`
	GroupBy       []string `json:"groupBy"` // required as an array, not null
}

// search posts a /api/v2/search request for the [start, end] window.
func (s *clickstack) search(where, whereLang, sel, orderBy string, maxResults int) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		return s.call(ctx, "/api/v2/search", func(src string) any {
			return csSearchReq{
				SourceID:      src,
				StartTime:     start.UTC().Format(time.RFC3339Nano),
				EndTime:       end.UTC().Format(time.RFC3339Nano),
				Where:         where,
				WhereLanguage: whereLang,
				Select:        sel,
				OrderBy:       orderBy,
				MaxResults:    maxResults,
			}
		})
	}
}

// series posts a /api/v2/charts/series count aggregation for the window.
func (s *clickstack) series(where, whereLang string, groupBy []string, granularity string) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	if groupBy == nil {
		groupBy = []string{}
	}
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		return s.call(ctx, "/api/v2/charts/series", func(src string) any {
			return csSeriesReq{
				Series:      []csSeries{{SourceID: src, AggFn: "count", Where: where, WhereLanguage: whereLang, GroupBy: groupBy}},
				StartTime:   start.UnixMilli(),
				EndTime:     end.UnixMilli(),
				Granularity: granularity,
			}
		})
	}
}

// call ensures auth + source id, marshals the request built from the source id,
// posts it with the Bearer key, and retries once on a 401 after re-bootstrapping.
func (s *clickstack) call(ctx context.Context, path string, build func(sourceID string) any) ([]byte, error) {
	var lastErr error
	for range 2 {
		key, src, err := s.ensure(ctx)
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(build(src))
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", s.addr+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			s.reset()
			lastErr = fmt.Errorf("POST %s: status 401: %s", path, truncate(string(rb), 200))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, truncate(string(rb), 300))
		}
		return rb, nil
	}
	return nil, lastErr
}

// ensure returns a cached (accessKey, sourceID), bootstrapping them on first use:
// register the first user (or log in if the team already exists) to get a session
// cookie, read the user's accessKey from /me, then find the "log" source id.
func (s *clickstack) ensure(ctx context.Context) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accessKey != "" && s.sourceID != "" {
		return s.accessKey, s.sourceID, nil
	}
	// HyperDX auth: best-effort register (creates the first user on a fresh deploy;
	// 409 if the team already exists), then login for a session cookie, then read
	// the access key from /me. login answers 303, so don't follow redirects. The
	// session cookie is scoped Domain=localhost, so a cookie jar would drop it for
	// our service host - we capture connect.sid from the login response and send it
	// on /me by hand. The access key itself is a long-lived Bearer token used for
	// every /api/v2 query.
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	reg := fmt.Sprintf(`{"email":%q,"password":%q,"confirmPassword":%q}`, clickstackEmail, clickstackPassword, clickstackPassword)
	_, _, _ = csAuthPost(ctx, client, s.addr+"/register/password", []byte(reg)) // best-effort: creates the user if none

	login := fmt.Sprintf(`{"email":%q,"password":%q}`, clickstackEmail, clickstackPassword)
	status, sid, err := clickstackLogin(ctx, client, s.addr+"/login/password", []byte(login))
	if err != nil {
		return "", "", fmt.Errorf("clickstack login: %w", err)
	}
	if sid == "" {
		return "", "", fmt.Errorf("clickstack login: status %d: no connect.sid cookie", status)
	}

	key, err := clickstackMe(ctx, client, s.addr, sid)
	if err != nil {
		return "", "", err
	}
	src, err := clickstackLogSource(ctx, s.addr, key)
	if err != nil {
		return "", "", err
	}
	s.accessKey = key
	s.sourceID = src
	return key, src, nil
}

func (s *clickstack) reset() {
	s.mu.Lock()
	s.accessKey = ""
	s.sourceID = ""
	s.mu.Unlock()
}

func csAuthPost(ctx context.Context, client *http.Client, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, rb, nil
}

// clickstackLogin posts credentials and returns the connect.sid session cookie
// value from the response (HyperDX answers 303 + Set-Cookie on success); we
// only keep the value.
func clickstackLogin(ctx context.Context, client *http.Client, url string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return resp.StatusCode, "", fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	for _, c := range resp.Cookies() {
		if c.Name == "connect.sid" {
			return resp.StatusCode, c.Value, nil
		}
	}
	return resp.StatusCode, "", nil
}

// clickstackMe reads the user's access key from GET /me, sending the captured
// session cookie by hand.
func clickstackMe(ctx context.Context, client *http.Client, addr, sid string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", addr+"/me", nil)
	if err != nil {
		return "", err
	}
	req.AddCookie(&http.Cookie{Name: "connect.sid", Value: sid})
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET /me: status %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var r struct {
		AccessKey string `json:"accessKey"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return "", fmt.Errorf("GET /me: parse: %w", err)
	}
	if r.AccessKey == "" {
		return "", fmt.Errorf("GET /me: no accessKey: %s", truncate(string(rb), 200))
	}
	return r.AccessKey, nil
}

// clickstackLogSource lists sources and returns the id of the one with kind "log".
func clickstackLogSource(ctx context.Context, addr, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", addr+"/api/v2/sources", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET /api/v2/sources: status %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var r struct {
		Data []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return "", fmt.Errorf("GET /api/v2/sources: parse: %w", err)
	}
	for _, src := range r.Data {
		if src.Kind == "log" {
			return src.ID, nil
		}
	}
	return "", fmt.Errorf("no log source in /api/v2/sources: %s", truncate(string(rb), 200))
}

// csSearchCheck validates a /api/v2/search response carries at least minRows rows.
func csSearchCheck(minRows int) func(body []byte) error {
	return func(body []byte) error {
		var r struct {
			Rows int               `json:"rows"`
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return fmt.Errorf("parse clickstack search: %w: %s", err, truncate(string(body), 200))
		}
		n := r.Rows
		if n == 0 {
			n = len(r.Data)
		}
		if n < minRows {
			return fmt.Errorf("clickstack search has %d rows; want at least %d", n, minRows)
		}
		return nil
	}
}

// csSeriesCheck validates a /api/v2/charts/series response carries at least
// minPoints data points.
func csSeriesCheck(minPoints int) func(body []byte) error {
	return func(body []byte) error {
		var r struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return fmt.Errorf("parse clickstack series: %w: %s", err, truncate(string(body), 200))
		}
		if len(r.Data) < minPoints {
			return fmt.Errorf("clickstack series has %d points; want at least %d", len(r.Data), minPoints)
		}
		return nil
	}
}

package systems

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// signoz benchmarks the SigNoz stack.
// It has 2 different endpoints for ingestion and queries,
// addr holds whichever endpoint is relevant for the current benchmark phase.
type signoz struct {
	addr string

	// querier-only bearer token for authentication
	mu    sync.Mutex
	token string
}

// SigNoz answers queries only after its first org and user exist. They are
// created with these credentials when SigNoz is installed, so the two must match.
const (
	signozEmail    = "logs-bench@example.com"
	signozPassword = "LogsBenchmark1!"
)

func (s *signoz) WaitReady() error {
	return waitHTTP(s.addr, func(*http.Response, []byte) error { return nil })
}

func (s *signoz) PrepareBatch(entries []LogEntry) (Batch, error) {
	return otlpBatch(entries)
}

func (s *signoz) Send(ctx context.Context, b Batch) (int, error) {
	_, _, retries, err := doReq(ctx, "POST", s.addr+"/v1/logs", otlpContentType, b.Body)
	return retries, err
}

// Queries are sent to Signoz via its API service.
// Every call carries a bearer token from the session API; a 401 refreshes it.

func (s *signoz) Queries(p QueryParams) []Query {
	app := strings.ReplaceAll(p.App, "'", "''")
	count := []szAgg{{Expression: "count()", Alias: "hits"}}
	newestFirst := []szOrder{{Key: szKey{Name: "timestamp"}, Direction: "desc"}}
	return []Query{
		{
			// Exploratory: newest 100 error lines.
			Name:   "recent_errors",
			Window: 5 * time.Minute,
			Do:     s.run("raw", szSpec{Filter: &szFilter{Expression: `severity_text = 'error'`}, Order: newestFirst, Limit: 100}),
			Check:  signozCheck(0),
		},
		{
			// Analytical: error count grouped by app.
			Name:   "errors_by_app",
			Window: time.Hour,
			Do:     s.run("scalar", szSpec{Aggregations: count, Filter: &szFilter{Expression: `severity_text = 'error'`}, GroupBy: []szKey{{Name: "service.name", FieldContext: "resource"}}}),
			Check:  signozCheck(1),
		},
		{
			// Scoped: one service's error count.
			Name:   "app_errors",
			Window: time.Hour,
			Do:     s.run("scalar", szSpec{Aggregations: count, Filter: &szFilter{Expression: fmt.Sprintf(`severity_text = 'error' AND resource.service.name = '%s'`, app)}}),
			Check:  signozCheck(1),
		},
		{
			// Needle in haystack
			Name:   "needle",
			Window: 24 * time.Hour,
			Do:     s.run("raw", szSpec{Filter: &szFilter{Expression: `body CONTAINS '7f3c9a2e-bench-needle'`}, Limit: 10}),
			Check:  signozCheck(0),
		},
		{
			// Common token: a frequent word
			Name:   "common_token",
			Window: time.Hour,
			Do:     s.run("raw", szSpec{Filter: &szFilter{Expression: `body CONTAINS 'program'`}, Order: newestFirst, Limit: 100}),
			Check:  signozCheck(1),
		},
		{
			// Time bucketing: per-minute counts.
			Name:   "time_buckets",
			Window: time.Hour,
			Do:     s.run("time_series", szSpec{Aggregations: count, StepInterval: "60s"}),
			Check:  signozCheck(1),
		},
	}
}

// StoredRows counts all stored logs with a scalar count() over storedRowsWindow.
func (s *signoz) StoredRows(ctx context.Context) (*int64, error) {
	end := time.Now()
	start := end.Add(-storedRowsWindow)
	do := s.run("scalar", szSpec{Aggregations: []szAgg{{Expression: "count()", Alias: "hits"}}})
	body, err := do(ctx, start, end)
	if err != nil {
		return nil, err
	}
	return signozStoredRows(body)
}

// signozStoredRows extracts the scalar count from a v5 scalar response: a
// no-groupBy count is in results[].data[0][0], with the aggregation series as a
// fallback.
func signozStoredRows(body []byte) (*int64, error) {
	var r struct {
		Data struct {
			Data struct {
				Results []struct {
					Data         [][]json.RawMessage `json:"data"`
					Aggregations []struct {
						Series []struct {
							Values []struct {
								Value json.RawMessage `json:"value"`
							} `json:"values"`
						} `json:"series"`
					} `json:"aggregations"`
				} `json:"results"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse signoz stored-rows response: %w: %s", err, truncate(string(body), 200))
	}
	for _, res := range r.Data.Data.Results {
		if len(res.Data) > 0 && len(res.Data[0]) > 0 {
			if n, err := jsonInt(res.Data[0][0]); err == nil {
				return &n, nil
			}
		}
		for _, a := range res.Aggregations {
			for _, se := range a.Series {
				if len(se.Values) > 0 {
					if n, err := jsonInt(se.Values[0].Value); err == nil {
						return &n, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf("signoz stored rows: no count in response: %s", truncate(string(body), 300))
}

// szReq is the v5 query_range request shape (builder query over logs). run() fills
// start/end/requestType and the spec; Signoz supports multiple queries in one request,
// but we send a single query. E.g. recent_errors marshals to:
//
//	{
//	  "schemaVersion": "v1",
//	  "start": 1755680100000,
//	  "end": 1755680400000,
//	  "requestType": "raw",
//	  "compositeQuery": {
//	    "queries": [
//	      {
//	        "type": "builder_query",
//	        "spec": {
//	          "name": "A",
//	          "signal": "logs",
//	          "filter": { "expression": "severity_text = 'error'" },
//	          "order": [ { "key": { "name": "timestamp" }, "direction": "desc" } ],
//	          "limit": 100
//	        }
//	      }
//	    ]
//	  }
//	}
type szReq struct {
	SchemaVersion  string      `json:"schemaVersion"`
	Start          int64       `json:"start"` // epoch milliseconds
	End            int64       `json:"end"`
	RequestType    string      `json:"requestType"` // raw | scalar | time_series
	CompositeQuery szComposite `json:"compositeQuery"`
}
type szComposite struct {
	Queries []szEnvelope `json:"queries"`
}
type szEnvelope struct {
	Type string `json:"type"` // "builder_query"
	Spec szSpec `json:"spec"`
}
type szSpec struct {
	Name         string    `json:"name"`
	Signal       string    `json:"signal"` // "logs"
	StepInterval string    `json:"stepInterval,omitempty"`
	Aggregations []szAgg   `json:"aggregations,omitempty"`
	Filter       *szFilter `json:"filter,omitempty"`
	GroupBy      []szKey   `json:"groupBy,omitempty"`
	Order        []szOrder `json:"order,omitempty"`
	Limit        int       `json:"limit,omitempty"`
}
type szAgg struct {
	Expression string `json:"expression"`
	Alias      string `json:"alias,omitempty"`
}
type szFilter struct {
	Expression string `json:"expression"`
}
type szKey struct {
	Name         string `json:"name"`
	FieldContext string `json:"fieldContext,omitempty"`
}
type szOrder struct {
	Key       szKey  `json:"key"`
	Direction string `json:"direction"`
}

// run builds a builder query for the [start, end] window and posts it.
func (s *signoz) run(requestType string, spec szSpec) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		spec.Name = "A"
		spec.Signal = "logs"
		body, err := json.Marshal(szReq{
			SchemaVersion:  "v1",
			Start:          start.UnixMilli(),
			End:            end.UnixMilli(),
			RequestType:    requestType,
			CompositeQuery: szComposite{Queries: []szEnvelope{{Type: "builder_query", Spec: spec}}},
		})
		if err != nil {
			return nil, err
		}
		return s.queryRange(ctx, body)
	}
}

// queryRange posts to /api/v5/query_range with a bearer token
// refreshing the token once on a 401.
func (s *signoz) queryRange(ctx context.Context, reqBody []byte) ([]byte, error) {
	var lastErr error
	url := s.addr + "/api/v5/query_range"
	for range 2 {
		tok, err := s.ensureToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			s.resetToken()
			lastErr = fmt.Errorf("POST /api/v5/query_range: status 401: %s", truncate(string(rb), 200))
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("POST /api/v5/query_range: status %d: %s", resp.StatusCode, truncate(string(rb), 300))
		}
		return rb, nil
	}
	return nil, lastErr
}

func (s *signoz) ensureToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	orgID, err := s.orgID(ctx)
	if err != nil {
		return "", fmt.Errorf("signoz org id: %w", err)
	}
	body := fmt.Sprintf(`{"email":%q,"password":%q,"orgId":%q}`, signozEmail, signozPassword, orgID)
	rb, err := s.postJSON(ctx, "/api/v2/sessions/email_password", []byte(body))
	if err != nil {
		return "", fmt.Errorf("signoz login: %w", err)
	}
	var r struct {
		AccessToken string `json:"accessToken"`
		Data        struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return "", fmt.Errorf("signoz login: parse: %w", err)
	}
	tok := r.AccessToken
	if tok == "" {
		tok = r.Data.AccessToken
	}
	if tok == "" {
		return "", fmt.Errorf("signoz login: no accessToken: %s", truncate(string(rb), 200))
	}
	s.token = tok
	return tok, nil
}

func (s *signoz) resetToken() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

// orgID discovers the (single) org id via the session context endpoint, which
// requires the email as a query param (and needs no auth).
func (s *signoz) orgID(ctx context.Context) (string, error) {
	u := s.addr + "/api/v2/sessions/context?email=" + url.QueryEscape(signozEmail)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET /api/v2/sessions/context: status %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var r struct {
		Orgs []struct {
			ID string `json:"id"`
		} `json:"orgs"`
		Data struct {
			Orgs []struct {
				ID string `json:"id"`
			} `json:"orgs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return "", err
	}
	orgs := r.Orgs
	if len(orgs) == 0 {
		orgs = r.Data.Orgs
	}
	if len(orgs) == 0 || orgs[0].ID == "" {
		return "", fmt.Errorf("no org in session context: %s", truncate(string(rb), 200))
	}
	return orgs[0].ID, nil
}

func (s *signoz) postJSON(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", s.addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, truncate(string(rb), 200))
	}
	return rb, nil
}

// signozCheck validates the query_range response (data.data.results) carries at
// least minRows items across raw rows, scalar table rows, and time-series points.
func signozCheck(minRows int) func(body []byte) error {
	return func(body []byte) error {
		var r struct {
			Data struct {
				Data struct {
					Results []struct {
						Rows         []json.RawMessage   `json:"rows"`
						Data         [][]json.RawMessage `json:"data"`
						Aggregations []struct {
							Series []struct {
								Values []json.RawMessage `json:"values"`
							} `json:"series"`
						} `json:"aggregations"`
					} `json:"results"`
				} `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			return fmt.Errorf("parse signoz response: %w: %s", err, truncate(string(body), 200))
		}
		n := 0
		for _, res := range r.Data.Data.Results {
			n += len(res.Rows) + len(res.Data)
			for _, a := range res.Aggregations {
				for _, se := range a.Series {
					n += len(se.Values)
				}
			}
		}
		if n < minRows {
			return fmt.Errorf("signoz response has %d rows/points; want at least %d", n, minRows)
		}
		return nil
	}
}

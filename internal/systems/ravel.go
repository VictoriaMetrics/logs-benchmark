package systems

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ravelToken is the bearer token every Ravel instance maps to the benchmark
// tenant (--tenant-token bench=bench in deploy/systems/ravel-render.sh). The
// instances are reachable only inside the cluster.
const ravelToken = "bench"

// ravel ingests OTLP/HTTP protobuf at /v1/logs, the same request body SigNoz
// and ClickStack get, and queries the `logs` table over POST /api/v1/sql.
// A 2xx from /v1/logs means the lines are durable in object storage.
type ravel struct {
	addr string // e.g. http://ravel:4318
	// ingestMode, when set, goes out as x-ravel-ingest-mode on every write
	// ("buffered" acks once the lines are buffered, before they are durable).
	ingestMode string
}

func (s *ravel) header() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+ravelToken)
	if s.ingestMode != "" {
		h.Set("x-ravel-ingest-mode", s.ingestMode)
	}
	return h
}

func (s *ravel) WaitReady() error {
	return waitHTTP(s.addr+"/readyz", status2xx)
}

func (s *ravel) PrepareBatch(entries []LogEntry) (Batch, error) {
	return otlpBatch(entries)
}

func (s *ravel) Send(ctx context.Context, b Batch) (int, error) {
	_, _, retries, err := doReqHeader(ctx, "POST", s.addr+"/v1/logs", otlpContentType, s.header(), b.Body)
	return retries, err
}

// The generator's fields land in the `attrs` map: host and app as the
// resource attributes host.name and service.name, every other field as a
// record attribute. The level is severity_text and the message is body.
func (s *ravel) Queries(p QueryParams) []Query {
	app := strings.ReplaceAll(p.App, "'", "''")
	return []Query{
		{
			// Exploratory
			Name:   "recent_errors",
			Window: 5 * time.Minute,
			Do:     s.sql(`SELECT ts, body FROM logs WHERE severity_text = 'error' AND %s ORDER BY ts DESC LIMIT 100`),
			Check:  checkRavelRows(0),
		},
		{
			// Analytical
			Name:   "errors_by_app",
			Window: time.Hour,
			Do:     s.sql(`SELECT attrs['service.name'] AS app, count(*) AS hits FROM logs WHERE severity_text = 'error' AND %s GROUP BY 1`),
			Check:  checkRavelRows(1),
		},
		{
			// By stream
			Name:   "app_errors",
			Window: time.Hour,
			Do:     s.sql(`SELECT count(*) AS hits FROM logs WHERE attrs['service.name'] = '` + app + `' AND severity_text = 'error' AND %s`),
			Check:  checkRavelRows(1),
		},
		{
			// Needle in a haystack
			Name:   "needle",
			Window: 24 * time.Hour,
			Do:     s.sql(`SELECT ts, body FROM logs WHERE has_word(body, '7f3c9a2e-bench-needle') AND %s LIMIT 10`),
			Check:  checkRavelRows(0),
		},
		{
			// Common token: "program"
			Name:   "common_token",
			Window: time.Hour,
			Do:     s.sql(`SELECT ts, body FROM logs WHERE has_word(body, 'program') AND %s ORDER BY ts DESC LIMIT 100`),
			Check:  checkRavelRows(1),
		},
		{
			// Time bucketing
			Name:   "time_buckets",
			Window: time.Hour,
			Do:     s.sql(`SELECT date_bin(INTERVAL '1 minute', ts) AS minute, count(*) AS hits FROM logs WHERE %s GROUP BY 1`),
			Check:  checkRavelRows(1),
		},
	}
}

// sql returns a Do func for a statement whose single %s is replaced by the
// row filter on ts over [start, end). The request's start/end only bound which
// segments are listed, so the statement filters rows itself.
func (s *ravel) sql(stmt string) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		return s.runSQL(ctx, fmt.Sprintf(stmt, ravelTsFilter(start, end)), start, end)
	}
}

func ravelTsFilter(start, end time.Time) string {
	return fmt.Sprintf("ts >= to_timestamp_nanos(%d) AND ts < to_timestamp_nanos(%d)", start.UnixNano(), end.UnixNano())
}

func (s *ravel) runSQL(ctx context.Context, stmt string, start, end time.Time) ([]byte, error) {
	req, err := json.Marshal(map[string]any{
		"query": stmt,
		"start": float64(start.UnixNano()) / 1e9,
		"end":   float64(end.UnixNano()) / 1e9,
	})
	if err != nil {
		return nil, err
	}
	return doQueryHeader(ctx, "POST", s.addr+"/api/v1/sql", "application/json", s.header(), req)
}

// ravelSQLResponse is the envelope of POST /api/v1/sql.
type ravelSQLResponse struct {
	Status string `json:"status"`
	Data   struct {
		Rows [][]json.RawMessage `json:"rows"`
	} `json:"data"`
}

func parseRavelSQL(body []byte) (ravelSQLResponse, error) {
	var r ravelSQLResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return r, fmt.Errorf("parse sql response: %w: %s", err, truncate(string(body), 200))
	}
	if r.Status != "success" {
		return r, fmt.Errorf("sql status %q: %s", r.Status, truncate(string(body), 200))
	}
	return r, nil
}

// StoredRows counts the rows a SQL scan of `logs` sees over storedRowsWindow.
// A row is visible only once it is committed to object storage, so in
// buffered mode too this counts durable rows, never acked-but-buffered ones.
func (s *ravel) StoredRows(ctx context.Context) (*int64, error) {
	end := time.Now()
	start := end.Add(-storedRowsWindow)
	body, err := s.runSQL(ctx, "SELECT count(*) AS n FROM logs", start, end)
	if err != nil {
		return nil, err
	}
	return ravelStoredRows(body)
}

func ravelStoredRows(body []byte) (*int64, error) {
	r, err := parseRavelSQL(body)
	if err != nil {
		return nil, err
	}
	if len(r.Data.Rows) != 1 || len(r.Data.Rows[0]) != 1 {
		return nil, fmt.Errorf("count response: want one row of one column: %s", truncate(string(body), 200))
	}
	n, err := jsonInt(r.Data.Rows[0][0])
	if err != nil {
		return nil, fmt.Errorf("count response: %w", err)
	}
	return &n, nil
}

// checkRavelRows returns a Check func: a success envelope with at least minRows rows.
func checkRavelRows(minRows int) func(body []byte) error {
	return func(body []byte) error {
		r, err := parseRavelSQL(body)
		if err != nil {
			return err
		}
		if len(r.Data.Rows) < minRows {
			return fmt.Errorf("response has %d rows; want at least %d", len(r.Data.Rows), minRows)
		}
		return nil
	}
}

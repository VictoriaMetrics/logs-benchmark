package systems

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/logs-benchmark/internal/source/synthetic"
)

// elasticsearch writes to Elasticsearch's default setup for logs: the
// built-in logs-*-* data stream template (LogsDB mode in 9.x).
type elasticsearch struct {
	addr string // e.g. http://es-es-http:9200
}

const esTarget = "logs-bench-default"

func (s *elasticsearch) WaitReady() error {
	return waitHTTP(s.addr+"/_cluster/health?wait_for_status=yellow&timeout=5s", esClusterReady)
}

func esClusterReady(r *http.Response, body []byte) error {
	if err := status2xx(r, body); err != nil {
		return err
	}
	var health struct {
		Status   string `json:"status"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(body), &health); err != nil {
		return fmt.Errorf("parse cluster health response: %w: %s", err, truncate(string(body), 200))
	}
	if health.TimedOut {
		return fmt.Errorf("cluster health timed out: %s", truncate(string(body), 200))
	}
	switch health.Status {
	case "green", "yellow":
		return nil
	case "":
		return fmt.Errorf("cluster health response missing status: %s", truncate(string(body), 200))
	default:
		return fmt.Errorf("cluster health status %q", health.Status)
	}
}

// Sample ECS log lines (NDJSON):
//
//	{"create":{}}
//	{"@timestamp":"2026-08-20T10:00:00.123456789Z","host":{"name":"web-01"},"app":"api","level":"error","message":"connection reset by peer"}
//	{"create":{}}
//	{"@timestamp":"2026-08-20T10:00:00.987654321Z","host":{"name":"web-02"},"app":"db","level":"info","message":"program executed \"vacuum\" ok"}
//
// Every line also carries the host and app constant fields (zone, namespace, ...)
// and the app's per-line fields (cache_status, request_id, ...).

var bulkAction = []byte(`{"create":{}}` + "\n")

// appendECS appends rec to dst in the minimal ECS shape the logs template requires (@timestamp, host as an object).
func appendECS(dst []byte, rec synthetic.Record, ts time.Time) []byte {
	dst = append(dst, `{"@timestamp":"`...)
	dst = ts.AppendFormat(dst, time.RFC3339Nano)
	dst = append(dst, `","host":{"name":`...)
	dst = synthetic.AppendJSONString(dst, rec.Host.Name)
	dst = append(dst, '}')
	dst = appendECSField(dst, "app", rec.App.Name)
	dst = appendECSField(dst, "level", rec.Level)
	dst = appendECSField(dst, "message", rec.Message)
	dst = append(dst, rec.Host.ConstJSON...)
	dst = append(dst, rec.App.ConstJSON...)
	dst = synthetic.AppendLineFields(dst, rec)
	return append(dst, '}')
}

func appendECSField(dst []byte, key, val string) []byte {
	dst = append(dst, ',')
	dst = synthetic.AppendJSONString(dst, key)
	dst = append(dst, ':')
	return synthetic.AppendJSONString(dst, val)
}

func (s *elasticsearch) PrepareBatch(entries []LogEntry) (Batch, error) {
	return esBulkBatch(entries), nil
}

// esBulkBatch encodes each record to ECS and prefixes the bulk create action.
func esBulkBatch(entries []LogEntry) Batch {
	buf := make([]byte, 0, len(entries)*1024)
	for _, e := range entries {
		buf = append(buf, bulkAction...)
		buf = appendECS(buf, e.Rec, e.Ts)
		buf = append(buf, '\n')
	}
	return Batch{Body: buf, Lines: len(entries)}
}

func (s *elasticsearch) Send(ctx context.Context, b Batch) (int, error) {
	return esBulkSend(ctx, s.addr+"/"+esTarget+"/_bulk", b)
}

// esBulkSend posts a bulk request to bulkURL.
// _bulk returns HTTP 200 even when individual items fail.
// So we collect items failing with 429 and retry them until the retry budget runs out or all items succeed.
func esBulkSend(ctx context.Context, bulkURL string, b Batch) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, maxRetryDuration)
	defer cancel()
	body := b.Body
	retries := 0
	backoff := 100 * time.Millisecond
	for {
		respBody, _, r, err := doReq(ctx, "POST", bulkURL, "application/x-ndjson", body)
		retries += r
		if err != nil {
			return retries, err
		}
		var resp struct {
			Errors bool `json:"errors"`
			Items  []map[string]struct {
				Status int `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal(respBody, &resp); err != nil {
			return retries, fmt.Errorf("parse bulk response: %w: %s", err, truncate(string(respBody), 300))
		}
		if !resp.Errors {
			return retries, nil
		}
		// collect retryable (429) items; any other failure is permanent
		lines := bytes.Split(bytes.TrimSuffix(body, []byte("\n")), []byte("\n"))
		var retry bytes.Buffer
		permanent := 0
		for i, item := range resp.Items {
			for _, st := range item {
				switch {
				case st.Status >= 200 && st.Status < 300:
				case st.Status == 429 && 2*i+1 < len(lines):
					retry.Write(lines[2*i])
					retry.WriteByte('\n')
					retry.Write(lines[2*i+1])
					retry.WriteByte('\n')
				default:
					permanent++
				}
			}
		}
		if permanent > 0 {
			return retries, fmt.Errorf("bulk response contains %d permanent item-level errors: %s", permanent, truncate(string(respBody), 300))
		}
		if retry.Len() == 0 {
			return retries, nil
		}
		retries++
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return retries, fmt.Errorf("bulk items still rejected with 429: %w", ctx.Err())
		}
		backoff = min(backoff*2, 5*time.Second)
		body = retry.Bytes()
	}
}

// The six benchmark query bodies, one per shape.
// %START%/%END% are filled per fire by search();
// %APP% in esQAppErrors is filled once in esQuerySet.
// All are window-scoped by the range clause.
const esQRecentErrors = `{
  "size": 100,
  "track_total_hits": false,
  "sort": [{"@timestamp": "desc"}],
  "query": {"bool": {"filter": [
    {"range": {"@timestamp": {"gte": "%START%", "lte": "%END%"}}},
    {"term": {"level": "error"}}
  ]}}
}`

const esQErrorsByApp = `{
  "size": 0,
  "query": {"bool": {"filter": [
    {"range": {"@timestamp": {"gte": "%START%", "lte": "%END%"}}},
    {"term": {"level": "error"}}
  ]}},
  "aggs": {"apps": {"terms": {"field": "app"}}}
}`

const esQAppErrors = `{
  "query": {"bool": {"filter": [
    {"range": {"@timestamp": {"gte": "%START%", "lte": "%END%"}}},
    {"term": {"app": %APP%}},
    {"term": {"level": "error"}}
  ]}}
}`

const esQNeedle = `{
  "size": 10,
  "query": {"bool": {"filter": [
    {"range": {"@timestamp": {"gte": "%START%", "lte": "%END%"}}},
    {"match_phrase": {"message": "7f3c9a2e-bench-needle"}}
  ]}}
}`

const esQCommonToken = `{
  "size": 100,
  "track_total_hits": false,
  "sort": [{"@timestamp": "desc"}],
  "query": {"bool": {"filter": [
    {"range": {"@timestamp": {"gte": "%START%", "lte": "%END%"}}},
    {"match_phrase": {"message": "program"}}
  ]}}
}`

const esQTimeBuckets = `{
  "size": 0,
  "query": {"bool": {"filter": [
    {"range": {"@timestamp": {"gte": "%START%", "lte": "%END%"}}}
  ]}},
  "aggs": {"buckets": {"date_histogram": {"field": "@timestamp", "fixed_interval": "1m"}}}
}`

func (s *elasticsearch) Queries(p QueryParams) []Query {
	return esQuerySet(s.addr, esTarget, p.App)
}

func (s *elasticsearch) StoredRows(ctx context.Context) (*int64, error) {
	return esStoredRows(ctx, s.addr, esTarget)
}

// esStoredRows counts all documents via the _count API (match_all, no time
// filter). Shared by Elasticsearch and OpenSearch.
func esStoredRows(ctx context.Context, addr, target string) (*int64, error) {
	body, err := doQuery(ctx, "GET", addr+"/"+target+"/_count", "", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parse _count response: %w: %s", err, truncate(string(body), 200))
	}
	return &r.Count, nil
}

// esQuerySet is the benchmark query set for any Elasticsearch-DSL system
func esQuerySet(addr, target, app string) []Query {
	searchPath := addr + "/" + target + "/_search"
	countPath := addr + "/" + target + "/_count"
	// search fills the %START%/%END% placeholders and posts the body.
	search := func(reqURL, bodyTmpl string) func(ctx context.Context, start, end time.Time) ([]byte, error) {
		return func(ctx context.Context, start, end time.Time) ([]byte, error) {
			body := strings.NewReplacer(
				"%START%", start.UTC().Format(time.RFC3339Nano),
				"%END%", end.UTC().Format(time.RFC3339Nano),
			).Replace(bodyTmpl)
			return doQuery(ctx, "POST", reqURL, "application/json", []byte(body))
		}
	}
	// app is known here (not per-fire), so bake it in once; strconv.Quote makes it
	// a JSON string. %START%/%END% stay for search().
	appErrors := strings.NewReplacer("%APP%", strconv.Quote(app)).Replace(esQAppErrors)
	return []Query{
		{
			Name:   "recent_errors",
			Window: 5 * time.Minute,
			Do:     search(searchPath, esQRecentErrors),
			Check:  esCheckHits(0),
		},
		{
			Name:   "errors_by_app",
			Window: time.Hour,
			Do:     search(searchPath, esQErrorsByApp),
			Check:  esCheckBuckets("apps", 1),
		},
		{
			Name:   "app_errors",
			Window: time.Hour,
			Do:     search(countPath, appErrors),
			Check:  esCheckCount,
		},
		{
			Name:   "needle",
			Window: 24 * time.Hour,
			Do:     search(searchPath, esQNeedle),
			Check:  esCheckHits(0),
		},
		{
			Name:   "common_token",
			Window: time.Hour,
			Do:     search(searchPath, esQCommonToken),
			Check:  esCheckHits(1),
		},
		{
			Name:   "time_buckets",
			Window: time.Hour,
			Do:     search(searchPath, esQTimeBuckets),
			Check:  esCheckBuckets("buckets", 1),
		},
	}
}

// esCheckHits returns a Check func: the response has a hits section with at
// least minHits entries.
func esCheckHits(minHits int) func(body []byte) error {
	return func(body []byte) error {
		var resp struct {
			Hits *struct {
				Hits []json.RawMessage `json:"hits"`
			} `json:"hits"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parse search response: %w: %s", err, truncate(string(body), 200))
		}
		if resp.Hits == nil {
			return fmt.Errorf("search response has no hits section: %s", truncate(string(body), 200))
		}
		if len(resp.Hits.Hits) < minHits {
			return fmt.Errorf("response has %d hits; want at least %d", len(resp.Hits.Hits), minHits)
		}
		return nil
	}
}

// esCheckBuckets returns a Check func: aggregation agg has at least
// minBuckets buckets.
func esCheckBuckets(agg string, minBuckets int) func(body []byte) error {
	return func(body []byte) error {
		var resp struct {
			Aggregations map[string]struct {
				Buckets []json.RawMessage `json:"buckets"`
			} `json:"aggregations"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parse aggregation response: %w: %s", err, truncate(string(body), 200))
		}
		buckets := resp.Aggregations[agg].Buckets
		if len(buckets) < minBuckets {
			return fmt.Errorf("aggregation %q has %d buckets; want at least %d", agg, len(buckets), minBuckets)
		}
		return nil
	}
}

// esCheckCount verifies a _count response carries the count field.
func esCheckCount(body []byte) error {
	var resp struct {
		Count *int64 `json:"count"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse count response: %w: %s", err, truncate(string(body), 200))
	}
	if resp.Count == nil {
		return fmt.Errorf("count response missing the count field: %s", truncate(string(body), 200))
	}
	return nil
}

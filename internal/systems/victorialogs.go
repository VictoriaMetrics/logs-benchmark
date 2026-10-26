package systems

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/logs-benchmark/internal/source/synthetic"
)

type victoriaLogs struct {
	addr string // e.g. http://victorialogs:9428
}

func (s *victoriaLogs) WaitReady() error {
	return waitHTTP(s.addr+"/health", status2xx)
}

// PrepareBatch writes the records as newline-delimited json (the shape VictoriaLogs ingests
// at /insert/jsonline), encoded straight from the struct into one buffer.
func (s *victoriaLogs) PrepareBatch(entries []LogEntry) (Batch, error) {
	buf := make([]byte, 0, len(entries)*1024)
	for _, e := range entries {
		buf = synthetic.AppendJSON(buf, e.Rec, e.Ts)
		buf = append(buf, '\n')
	}
	return Batch{Body: buf, Lines: len(entries)}, nil
}

func (s *victoriaLogs) Send(ctx context.Context, b Batch) (int, error) {
	reqURL := s.addr + "/insert/jsonline?_time_field=timestamp&_msg_field=message&_stream_fields=host,app"
	_, _, retries, err := doReq(ctx, "POST", reqURL, "application/x-ndjson", b.Body)
	return retries, err
}

// victoriaLogsLoki sends the same request as loki (snappy protobuf, labels
// {app, host}) to the Loki push endpoint of VictoriaLogs, to compare its cost
// with /insert/jsonline. The labels become the stream fields and the JSON line
// is parsed into fields; the entry carries the time, so the line's own
// timestamp field is dropped to store the same fields as victorialogs. Only
// the wire format differs, so the queries are victorialogs'.
type victoriaLogsLoki struct {
	victoriaLogs
}

func (s *victoriaLogsLoki) PrepareBatch(entries []LogEntry) (Batch, error) {
	return (&loki{}).PrepareBatch(entries)
}

func (s *victoriaLogsLoki) Send(ctx context.Context, b Batch) (int, error) {
	reqURL := s.addr + "/insert/loki/api/v1/push?_msg_field=message&ignore_fields=timestamp"
	_, _, retries, err := doReq(ctx, "POST", reqURL, "application/x-protobuf", b.Body)
	return retries, err
}

func (s *victoriaLogs) Queries(p QueryParams) []Query {
	return []Query{
		{
			// Exploratory
			Name:   "recent_errors",
			Window: 5 * time.Minute,
			Do:     s.logsql(`level:=error | sort by (_time) desc | limit 100`),
			Check:  checkJSONLines(0),
		},
		{
			// Analytical
			Name:   "errors_by_app",
			Window: time.Hour,
			Do:     s.logsql(`level:=error | stats by (app) count() hits`),
			Check:  checkJSONLines(1),
		},
		{
			// By stream
			Name:   "app_errors",
			Window: time.Hour,
			Do:     s.logsql(fmt.Sprintf(`{app=%q} level:=error | stats count() hits`, p.App)),
			Check:  checkJSONLines(1),
		},
		{
			// Needle in a haystack
			Name:   "needle",
			Window: 24 * time.Hour,
			Do:     s.logsql(`"7f3c9a2e-bench-needle" | limit 10`),
			Check:  checkJSONLines(0),
		},
		{
			// Common token: "program"
			Name:   "common_token",
			Window: time.Hour,
			Do:     s.logsql(`"program" | sort by (_time) desc | limit 100`),
			Check:  checkJSONLines(1),
		},
		{
			// Time bucketing
			Name:   "time_buckets",
			Window: time.Hour,
			Do:     s.logsql(`* | stats by (_time:1m) count() hits`),
			Check:  checkJSONLines(1),
		},
	}
}

func (s *victoriaLogs) logsql(q string) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		v := url.Values{}
		v.Set("query", q)
		v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
		v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
		return doQuery(ctx, "POST", s.addr+"/select/logsql/query", "application/x-www-form-urlencoded", []byte(v.Encode()))
	}
}

// StoredRows counts stored rows with `stats count()` over storedRowsWindow.
func (s *victoriaLogs) StoredRows(ctx context.Context) (*int64, error) {
	end := time.Now()
	start := end.Add(-storedRowsWindow)
	v := url.Values{}
	v.Set("query", `* | stats count() rows`)
	v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	body, err := doQuery(ctx, "POST", s.addr+"/select/logsql/query", "application/x-www-form-urlencoded", []byte(v.Encode()))
	if err != nil {
		return nil, err
	}
	// one JSON line, e.g. {"rows":"12345"}
	for ln := range bytes.SplitSeq(body, []byte("\n")) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(ln, &m); err != nil {
			return nil, fmt.Errorf("parse count response: %w: %s", err, truncate(string(ln), 200))
		}
		raw, ok := m["rows"]
		if !ok {
			return nil, fmt.Errorf("count response missing rows: %s", truncate(string(ln), 200))
		}
		n, err := jsonInt(raw)
		if err != nil {
			return nil, fmt.Errorf("count response rows: %w", err)
		}
		return &n, nil
	}
	return nil, fmt.Errorf("empty count response")
}

// checkJSONLines returns a Check func: valid JSON lines, at least minLines of them.
func checkJSONLines(minLines int) func(body []byte) error {
	return func(body []byte) error {
		lines := 0
		for ln := range bytes.SplitSeq(body, []byte("\n")) {
			ln = bytes.TrimSpace(ln)
			if len(ln) == 0 {
				continue
			}
			if !json.Valid(ln) {
				return fmt.Errorf("response line %d is not valid JSON: %s", lines+1, truncate(string(ln), 100))
			}
			lines++
		}
		if lines < minLines {
			return fmt.Errorf("response has %d lines; want at least %d", lines, minLines)
		}
		return nil
	}
}

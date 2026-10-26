package systems

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/easyproto"
	"github.com/golang/snappy"

	"github.com/VictoriaMetrics/logs-benchmark/internal/source/synthetic"
)

type loki struct {
	addr string // e.g. http://loki:3100
}

func (s *loki) WaitReady() error {
	return waitHTTP(s.addr+"/ready", status2xx)
}

var lokiMarshalerPool easyproto.MarshalerPool

// PrepareBatch encodes the records as a snappy-compressed protobuf PushRequest,
// with one stream per (host, app) labeled {app, host}. Each entry is the
// timestamp and the full JSON line. Loki decodes a protobuf push body as snappy,
// so the body cannot be sent uncompressed.
//
// Field numbers are from Loki's push.proto:
// https://github.com/grafana/loki/blob/ada4b7b8713385fbe9f5984a5a0aaaddf1a7b851/pkg/push/push.proto
func (s *loki) PrepareBatch(entries []LogEntry) (Batch, error) {
	m := lokiMarshalerPool.Get()
	defer lokiMarshalerPool.Put(m)

	req := m.MessageMarshaler()
	var labels, line []byte
	for _, stream := range groupByStream(entries) {
		st := req.AppendMessage(1) // PushRequest.streams
		labels = appendLokiLabels(labels[:0], stream[0].Rec)
		st.AppendBytes(1, labels) // StreamAdapter.labels
		for _, e := range stream {
			en := st.AppendMessage(2) // StreamAdapter.entries
			ns := e.Ts.UnixNano()
			ts := en.AppendMessage(1) // EntryAdapter.timestamp: google.protobuf.Timestamp
			ts.AppendInt64(1, ns/1e9)
			ts.AppendInt32(2, int32(ns%1e9))
			line = synthetic.AppendJSON(line[:0], e.Rec, e.Ts)
			en.AppendBytes(2, line) // EntryAdapter.line
		}
	}
	pb := m.Marshal(make([]byte, 0, len(entries)*1024))
	return Batch{Body: snappy.Encode(nil, pb), Lines: len(entries)}, nil
}

// appendLokiLabels appends the stream selector {app="...", host="..."}, the
// label set in the form Loki parses from StreamAdapter.labels.
func appendLokiLabels(dst []byte, rec synthetic.Record) []byte {
	dst = append(dst, `{app=`...)
	dst = strconv.AppendQuote(dst, rec.App.Name)
	dst = append(dst, `, host=`...)
	dst = strconv.AppendQuote(dst, rec.Host.Name)
	return append(dst, '}')
}

func (s *loki) Send(ctx context.Context, b Batch) (int, error) {
	// push returns 204 on success; doReq treats any 2xx as success.
	_, _, retries, err := doReq(ctx, "POST", s.addr+"/loki/api/v1/push", "application/x-protobuf", b.Body)
	return retries, err
}

// lokiJSON pushes the same streams and lines as loki in Loki's JSON push
// format, which shippers send when not configured for protobuf. Only the wire
// format differs, so the queries are loki's.
type lokiJSON struct {
	loki
}

// PrepareBatch encodes the records as a JSON PushRequest, one stream per
// (host, app):
//
//	{"streams":[{"stream":{"app":"...","host":"..."},"values":[["<unix ns>","<JSON line>"],...]},...]}
func (s *lokiJSON) PrepareBatch(entries []LogEntry) (Batch, error) {
	buf := make([]byte, 0, len(entries)*1200)
	var line []byte
	buf = append(buf, `{"streams":[`...)
	for i, stream := range groupByStream(entries) {
		if i > 0 {
			buf = append(buf, ',')
		}
		rec := stream[0].Rec
		buf = append(buf, `{"stream":{"app":`...)
		buf = synthetic.AppendJSONString(buf, rec.App.Name)
		buf = append(buf, `,"host":`...)
		buf = synthetic.AppendJSONString(buf, rec.Host.Name)
		buf = append(buf, `},"values":[`...)
		for j, e := range stream {
			if j > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, `["`...)
			buf = strconv.AppendInt(buf, e.Ts.UnixNano(), 10)
			buf = append(buf, `",`...)
			line = synthetic.AppendJSON(line[:0], e.Rec, e.Ts)
			buf = synthetic.AppendJSONString(buf, bytesutil.ToUnsafeString(line))
			buf = append(buf, ']')
		}
		buf = append(buf, `]}`...)
	}
	buf = append(buf, `]}`...)
	return Batch{Body: buf, Lines: len(entries)}, nil
}

func (s *lokiJSON) Send(ctx context.Context, b Batch) (int, error) {
	_, _, retries, err := doReq(ctx, "POST", s.addr+"/loki/api/v1/push", "application/json", b.Body)
	return retries, err
}

func (s *loki) Queries(p QueryParams) []Query {
	return []Query{
		{
			// Exploratory
			Name:   "recent_errors",
			Window: 5 * time.Minute,
			Do:     s.logRange(`{app=~".+"} | json | level="error"`, 100),
			Check:  lokiCheckEntries(0),
		},
		{
			// Analytical
			Name:   "errors_by_app",
			Window: time.Hour,
			Do:     s.instant(`sum by (app) (count_over_time({app=~".+"} | json | level="error" [1h]))`),
			Check:  lokiCheckSeries(1),
		},
		{
			// By stream
			Name:   "app_errors",
			Window: time.Hour,
			Do:     s.instant(fmt.Sprintf(`sum(count_over_time({app=%q} | json | level="error" [1h]))`, p.App)),
			Check:  lokiCheckSeries(1),
		},
		// needle (24h, token with zero hits) is intentionally omitted for Loki.
		// With no content index, Loki scans the whole 24h window and cannot
		// early-exit on zero matches, so it always exceeds the query timeout:
		// it would only ever record failures (never a latency) and would block
		// the querier warm-up.
		{
			// Common token: "program" is a common word in gofakeit-generated messages
			Name:   "common_token",
			Window: time.Hour,
			Do:     s.logRange(`{app=~".+"} |= "program"`, 100),
			Check:  lokiCheckEntries(1),
		},
		{
			// Time bucketing: the query for hit charts and dashboards
			Name:   "time_buckets",
			Window: time.Hour,
			Do:     s.rangeStep(`sum(count_over_time({app=~".+"}[1m]))`, "60s"),
			Check:  lokiCheckSeries(1),
		},
	}
}

// logRange runs a log query over [start, end], newest first, capped at limit.
func (s *loki) logRange(q string, limit int) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		v := url.Values{}
		v.Set("query", q)
		v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
		v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
		v.Set("limit", strconv.Itoa(limit))
		v.Set("direction", "backward")
		return doQuery(ctx, "POST", s.addr+"/loki/api/v1/query_range", "application/x-www-form-urlencoded", []byte(v.Encode()))
	}
}

// instant runs a metric query at end time; the lookback range lives in the query.
func (s *loki) instant(q string) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, _, end time.Time) ([]byte, error) {
		v := url.Values{}
		v.Set("query", q)
		v.Set("time", strconv.FormatInt(end.UnixNano(), 10))
		return doQuery(ctx, "POST", s.addr+"/loki/api/v1/query", "application/x-www-form-urlencoded", []byte(v.Encode()))
	}
}

// rangeStep runs a metric query over [start, end] bucketed by step.
func (s *loki) rangeStep(q, step string) func(ctx context.Context, start, end time.Time) ([]byte, error) {
	return func(ctx context.Context, start, end time.Time) ([]byte, error) {
		v := url.Values{}
		v.Set("query", q)
		v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
		v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
		v.Set("step", step)
		return doQuery(ctx, "POST", s.addr+"/loki/api/v1/query_range", "application/x-www-form-urlencoded", []byte(v.Encode()))
	}
}

// StoredRows: Loki has no cheap exact count (its index stats are only an estimate).
func (s *loki) StoredRows(_ context.Context) (*int64, error) {
	return nil, nil
}

// lokiCheckEntries returns a Check func: the streams result carries at least
// minEntries log lines in total.
func lokiCheckEntries(minEntries int) func(body []byte) error {
	return func(body []byte) error {
		var resp struct {
			Data struct {
				Result []struct {
					Values []json.RawMessage `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parse loki query response: %w: %s", err, truncate(string(body), 200))
		}
		n := 0
		for _, st := range resp.Data.Result {
			n += len(st.Values)
		}
		if n < minEntries {
			return fmt.Errorf("response has %d log entries; want at least %d", n, minEntries)
		}
		return nil
	}
}

// lokiCheckSeries returns a Check func: the metric result carries at least
// minSeries series (vector or matrix).
func lokiCheckSeries(minSeries int) func(body []byte) error {
	return func(body []byte) error {
		var resp struct {
			Data struct {
				Result []json.RawMessage `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return fmt.Errorf("parse loki metric response: %w: %s", err, truncate(string(body), 200))
		}
		if len(resp.Data.Result) < minSeries {
			return fmt.Errorf("response has %d series; want at least %d", len(resp.Data.Result), minSeries)
		}
		return nil
	}
}

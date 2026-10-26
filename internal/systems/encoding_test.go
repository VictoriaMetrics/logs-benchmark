package systems

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/VictoriaMetrics/easyproto"
	"github.com/golang/snappy"

	"github.com/VictoriaMetrics/logs-benchmark/internal/source/synthetic"
)

// testEntries returns three records over two streams, interleaved so that the
// encoders must group them: (host-a, app), (host-b, app), (host-a, app).
func testEntries() []LogEntry {
	hostA := &synthetic.HostInfo{Name: "host-a", Consts: []synthetic.Field{{Name: "zone", Value: "us-east1-b"}}}
	hostB := &synthetic.HostInfo{Name: "host-b", Consts: []synthetic.Field{{Name: "zone", Value: "us-east1-c"}}}
	for _, h := range []*synthetic.HostInfo{hostA, hostB} {
		h.ConstJSON = []byte(`,"zone":"` + h.Consts[0].Value + `"`)
	}
	app := &synthetic.AppInfo{
		Name:      `api "quoted"`,
		Consts:    []synthetic.Field{{Name: "replicas", Value: "3", Numeric: true}},
		ConstJSON: []byte(`,"replicas":3`),
		Lines: []synthetic.PoolField{
			{Name: "cache_status"},
			{Name: "duration_ms", Numeric: true},
		},
	}
	ts := time.Date(2026, 9, 27, 12, 0, 0, 123456789, time.UTC)
	rec := func(host *synthetic.HostInfo, level, msg, cache, dur string) synthetic.Record {
		return synthetic.Record{Host: host, App: app, Level: level, Message: msg, LineVals: []string{cache, dur}}
	}
	return []LogEntry{
		{Rec: rec(hostA, "info", "first", "hit", "12"), Ts: ts},
		{Rec: rec(hostB, "error", "second", "miss", "0"), Ts: ts.Add(time.Second)},
		{Rec: rec(hostA, "warning", "third", "bypass", "4999"), Ts: ts.Add(2 * time.Second)},
	}
}

func TestGroupByStream(t *testing.T) {
	entries := testEntries()
	groups := groupByStream(entries)
	var got [][]string
	for _, g := range groups {
		var msgs []string
		for _, e := range g {
			msgs = append(msgs, e.Rec.Message)
		}
		got = append(got, msgs)
	}
	want := [][]string{{"first", "third"}, {"second"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected groups: got %q; want %q", got, want)
	}
}

// otlpRecord is a decoded LogRecord; attribute values are string or int64.
type otlpRecord struct {
	TimeUnixNano   uint64
	SeverityNumber int32
	SeverityText   string
	Body           string
	Attrs          map[string]any
}

type otlpResourceLogs struct {
	Resource map[string]any
	Records  []otlpRecord
}

func TestOTLPBatch(t *testing.T) {
	entries := testEntries()
	b, err := otlpBatch(entries)
	if err != nil {
		t.Fatalf("otlpBatch: %v", err)
	}
	if b.Lines != len(entries) {
		t.Fatalf("unexpected Lines: got %d; want %d", b.Lines, len(entries))
	}
	got, err := decodeOTLP(b.Body)
	if err != nil {
		t.Fatalf("cannot decode the request: %v", err)
	}

	app := `api "quoted"`
	record := func(e LogEntry, sevNum int32, zone, cache string, dur int64) otlpRecord {
		return otlpRecord{
			TimeUnixNano:   uint64(e.Ts.UnixNano()),
			SeverityNumber: sevNum,
			SeverityText:   e.Rec.Level,
			Body:           e.Rec.Message,
			Attrs:          map[string]any{"zone": zone, "replicas": int64(3), "cache_status": cache, "duration_ms": dur},
		}
	}
	want := []otlpResourceLogs{
		{
			Resource: map[string]any{"host.name": "host-a", "service.name": app},
			Records: []otlpRecord{
				record(entries[0], 9, "us-east1-b", "hit", 12),
				record(entries[2], 13, "us-east1-b", "bypass", 4999),
			},
		},
		{
			Resource: map[string]any{"host.name": "host-b", "service.name": app},
			Records:  []otlpRecord{record(entries[1], 17, "us-east1-c", "miss", 0)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected request:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestOTLPBatchRejectsNonIntegerNumeric(t *testing.T) {
	entries := testEntries()
	entries[1].Rec.LineVals = []string{"miss", "1.5"}
	if _, err := otlpBatch(entries); err == nil {
		t.Fatalf("expected an error for a non-integer numeric field")
	}
}

// decodeOTLP decodes an ExportLogsServiceRequest, failing on any field the
// encoder is not expected to write.
func decodeOTLP(src []byte) ([]otlpResourceLogs, error) {
	var out []otlpResourceLogs
	err := forEachField(src, func(fc *easyproto.FieldContext) error {
		if fc.FieldNum != 1 {
			return fmt.Errorf("ExportLogsServiceRequest: unexpected field %d", fc.FieldNum)
		}
		data, _ := fc.MessageData()
		var rl otlpResourceLogs
		err := forEachField(data, func(fc *easyproto.FieldContext) error {
			data, ok := fc.MessageData()
			if !ok {
				return fmt.Errorf("ResourceLogs: field %d is not a message", fc.FieldNum)
			}
			switch fc.FieldNum {
			case 1:
				attrs, err := decodeOTLPAttrs(data, 1)
				rl.Resource = attrs
				return err
			case 2:
				return forEachField(data, func(fc *easyproto.FieldContext) error {
					if fc.FieldNum != 2 {
						return fmt.Errorf("ScopeLogs: unexpected field %d", fc.FieldNum)
					}
					data, _ := fc.MessageData()
					r, err := decodeOTLPRecord(data)
					rl.Records = append(rl.Records, r)
					return err
				})
			}
			return fmt.Errorf("ResourceLogs: unexpected field %d", fc.FieldNum)
		})
		out = append(out, rl)
		return err
	})
	return out, err
}

func decodeOTLPRecord(src []byte) (otlpRecord, error) {
	r := otlpRecord{Attrs: map[string]any{}}
	err := forEachField(src, func(fc *easyproto.FieldContext) error {
		var ok bool
		switch fc.FieldNum {
		case 1:
			r.TimeUnixNano, ok = fc.Fixed64()
		case 2:
			r.SeverityNumber, ok = fc.Enum()
		case 3:
			r.SeverityText, ok = fc.String()
		case 5:
			var data []byte
			if data, ok = fc.MessageData(); ok {
				v, err := decodeOTLPValue(data)
				if err != nil {
					return err
				}
				r.Body, ok = v.(string)
			}
		case 6:
			var data []byte
			if data, ok = fc.MessageData(); ok {
				k, v, err := decodeOTLPKeyValue(data)
				if err != nil {
					return err
				}
				r.Attrs[k] = v
			}
		default:
			return fmt.Errorf("LogRecord: unexpected field %d", fc.FieldNum)
		}
		if !ok {
			return fmt.Errorf("LogRecord: field %d has an unexpected type", fc.FieldNum)
		}
		return nil
	})
	return r, err
}

func decodeOTLPAttrs(src []byte, fieldNum uint32) (map[string]any, error) {
	attrs := map[string]any{}
	err := forEachField(src, func(fc *easyproto.FieldContext) error {
		if fc.FieldNum != fieldNum {
			return fmt.Errorf("unexpected field %d", fc.FieldNum)
		}
		data, _ := fc.MessageData()
		k, v, err := decodeOTLPKeyValue(data)
		attrs[k] = v
		return err
	})
	return attrs, err
}

func decodeOTLPKeyValue(src []byte) (string, any, error) {
	var key string
	var value any
	err := forEachField(src, func(fc *easyproto.FieldContext) error {
		switch fc.FieldNum {
		case 1:
			key, _ = fc.String()
			return nil
		case 2:
			data, _ := fc.MessageData()
			v, err := decodeOTLPValue(data)
			value = v
			return err
		}
		return fmt.Errorf("KeyValue: unexpected field %d", fc.FieldNum)
	})
	return key, value, err
}

// decodeOTLPValue decodes an AnyValue holding a string_value or an int_value.
func decodeOTLPValue(src []byte) (any, error) {
	var value any
	err := forEachField(src, func(fc *easyproto.FieldContext) error {
		var ok bool
		switch fc.FieldNum {
		case 1:
			value, ok = fc.String()
		case 3:
			value, ok = fc.Int64()
		default:
			return fmt.Errorf("AnyValue: unexpected field %d", fc.FieldNum)
		}
		if !ok {
			return fmt.Errorf("AnyValue: field %d has an unexpected type", fc.FieldNum)
		}
		return nil
	})
	return value, err
}

type lokiEntry struct {
	Seconds int64
	Nanos   int32
	Line    string
}

type lokiStream struct {
	Labels  string
	Entries []lokiEntry
}

func TestLokiPrepareBatch(t *testing.T) {
	entries := testEntries()
	b, err := (&loki{}).PrepareBatch(entries)
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if b.Lines != len(entries) {
		t.Fatalf("unexpected Lines: got %d; want %d", b.Lines, len(entries))
	}
	pb, err := snappy.Decode(nil, b.Body)
	if err != nil {
		t.Fatalf("the body is not snappy-compressed: %v", err)
	}
	got, err := decodeLokiPush(pb)
	if err != nil {
		t.Fatalf("cannot decode the request: %v", err)
	}

	entry := func(e LogEntry) lokiEntry {
		return lokiEntry{
			Seconds: e.Ts.Unix(),
			Nanos:   int32(e.Ts.Nanosecond()),
			Line:    string(synthetic.AppendJSON(nil, e.Rec, e.Ts)),
		}
	}
	want := []lokiStream{
		{Labels: `{app="api \"quoted\"", host="host-a"}`, Entries: []lokiEntry{entry(entries[0]), entry(entries[2])}},
		{Labels: `{app="api \"quoted\"", host="host-b"}`, Entries: []lokiEntry{entry(entries[1])}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected request:\ngot  %+v\nwant %+v", got, want)
	}
}

func TestLokiJSONPrepareBatch(t *testing.T) {
	entries := testEntries()
	b, err := (&lokiJSON{}).PrepareBatch(entries)
	if err != nil {
		t.Fatalf("PrepareBatch: %v", err)
	}
	if b.Lines != len(entries) {
		t.Fatalf("unexpected Lines: got %d; want %d", b.Lines, len(entries))
	}
	var got struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b.Body, &got); err != nil {
		t.Fatalf("the body is not valid JSON: %v\n%s", err, b.Body)
	}

	value := func(e LogEntry) [2]string {
		return [2]string{fmt.Sprint(e.Ts.UnixNano()), string(synthetic.AppendJSON(nil, e.Rec, e.Ts))}
	}
	type stream struct {
		Stream map[string]string
		Values [][2]string
	}
	want := []stream{
		{Stream: map[string]string{"app": `api "quoted"`, "host": "host-a"}, Values: [][2]string{value(entries[0]), value(entries[2])}},
		{Stream: map[string]string{"app": `api "quoted"`, "host": "host-b"}, Values: [][2]string{value(entries[1])}},
	}
	var gotStreams []stream
	for _, st := range got.Streams {
		gotStreams = append(gotStreams, stream{Stream: st.Stream, Values: st.Values})
	}
	if !reflect.DeepEqual(gotStreams, want) {
		t.Fatalf("unexpected request:\ngot  %+v\nwant %+v", gotStreams, want)
	}
}

// decodeLokiPush decodes a PushRequest, failing on any field the encoder is
// not expected to write.
func decodeLokiPush(src []byte) ([]lokiStream, error) {
	var out []lokiStream
	err := forEachField(src, func(fc *easyproto.FieldContext) error {
		if fc.FieldNum != 1 {
			return fmt.Errorf("PushRequest: unexpected field %d", fc.FieldNum)
		}
		data, _ := fc.MessageData()
		var st lokiStream
		err := forEachField(data, func(fc *easyproto.FieldContext) error {
			switch fc.FieldNum {
			case 1:
				st.Labels, _ = fc.String()
				return nil
			case 2:
				data, _ := fc.MessageData()
				var e lokiEntry
				err := forEachField(data, func(fc *easyproto.FieldContext) error {
					switch fc.FieldNum {
					case 1:
						data, _ := fc.MessageData()
						return forEachField(data, func(fc *easyproto.FieldContext) error {
							switch fc.FieldNum {
							case 1:
								e.Seconds, _ = fc.Int64()
								return nil
							case 2:
								e.Nanos, _ = fc.Int32()
								return nil
							}
							return fmt.Errorf("Timestamp: unexpected field %d", fc.FieldNum)
						})
					case 2:
						e.Line, _ = fc.String()
						return nil
					}
					return fmt.Errorf("EntryAdapter: unexpected field %d", fc.FieldNum)
				})
				st.Entries = append(st.Entries, e)
				return err
			}
			return fmt.Errorf("StreamAdapter: unexpected field %d", fc.FieldNum)
		})
		out = append(out, st)
		return err
	})
	return out, err
}

// forEachField calls f for every field of the protobuf message src.
func forEachField(src []byte, f func(fc *easyproto.FieldContext) error) error {
	var fc easyproto.FieldContext
	for len(src) > 0 {
		var err error
		src, err = fc.NextField(src)
		if err != nil {
			return err
		}
		if err := f(&fc); err != nil {
			return err
		}
	}
	return nil
}

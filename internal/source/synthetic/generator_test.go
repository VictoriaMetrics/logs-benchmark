package synthetic

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// generate returns n newline-framed lines with a fixed timestamp, encoded with
// enc, so output depends only on the seed.
func generate(g *Generator, enc Encoding, n int) []byte {
	ts := time.Unix(0, 0).UTC()
	var buf bytes.Buffer
	for range n {
		buf.Write(enc.Encode(nil, g.Next(), ts))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func TestDeterministic(t *testing.T) {
	c := NewFleet(1, 8, 4)
	a := generate(New(c, 1), JSON, 20_000)
	b := generate(New(c, 1), JSON, 20_000)
	if !bytes.Equal(a, b) {
		t.Fatal("same seed produced different output")
	}
	if bytes.Equal(a, generate(New(c, 42), JSON, 20_000)) {
		t.Fatal("different content seeds produced identical output")
	}
	ja, _ := json.Marshal(c)
	jb, _ := json.Marshal(NewFleet(1, 8, 4))
	if !bytes.Equal(ja, jb) {
		t.Fatal("same seed produced different fleets")
	}
}

// TestSchemaFixedPerStream proves every stream (host, app pair) keeps one
// field set for the whole run, sized 30-40 fields, with non-empty values.
func TestSchemaFixedPerStream(t *testing.T) {
	out := generate(New(NewFleet(1, 8, 4), 1), JSON, 10_000)
	streamKeys := map[string]map[string]bool{}
	for line := range bytes.SplitSeq(bytes.TrimSpace(out), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("invalid JSON line: %v\n%s", err, line)
		}
		if n := len(rec); n < 30 || n > 40 {
			t.Fatalf("line has %d fields; want 30-40:\n%s", n, line)
		}
		keys := map[string]bool{}
		for k, v := range rec {
			keys[k] = true
			if s, isStr := v.(string); isStr && s == "" {
				t.Fatalf("field %q is empty:\n%s", k, line)
			}
		}
		stream := rec["host"].(string) + "|" + rec["app"].(string)
		prev, seen := streamKeys[stream]
		if !seen {
			streamKeys[stream] = keys
			continue
		}
		if len(prev) != len(keys) {
			t.Fatalf("stream %s changed its field count", stream)
		}
		for k := range keys {
			if !prev[k] {
				t.Fatalf("stream %s gained field %q", stream, k)
			}
		}
	}
}

func TestJSONLinesAreValid(t *testing.T) {
	out := generate(New(NewFleet(1, 8, 4), 1), JSON, 1000)
	for line := range bytes.SplitSeq(bytes.TrimSpace(out), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("invalid JSON line: %v\n%s", err, line)
		}
	}
}

func TestNewPanicsOnEmptyFleet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New with an empty fleet did not panic")
		}
	}()
	New(&Fleet{}, 1)
}

func BenchmarkNext(b *testing.B) {
	g := New(NewFleet(1, 8, 4), 1)
	b.ReportAllocs()
	for b.Loop() {
		g.Next()
	}
}

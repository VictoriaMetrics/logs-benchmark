package synthetic

import (
	"encoding/json"
	"testing"
	"time"
)

// TestAppendJSONRoundTrips proves the hand-rolled AppendJSON produces valid JSON
// whose decoded values match the record, including escaping of quotes, backslashes
// and control characters.
func TestAppendJSONRoundTrips(t *testing.T) {
	ts := time.Date(2026, 8, 9, 12, 34, 56, 789, time.UTC)
	host := &HostInfo{
		Name:   "host-1",
		Consts: []Field{{Name: "zone", Value: "us-east1-b"}},
	}
	host.ConstJSON = constJSON(host.Consts)
	app := &AppInfo{
		Name: `api "quote" and \back`,
		Consts: []Field{
			{Name: "namespace", Value: `has "quote"`},
			{Name: "replicas", Value: "3", Numeric: true},
		},
		Lines: []PoolField{
			{Name: "cache_status"},
			{Name: "duration_ms", Numeric: true},
		},
	}
	app.ConstJSON = constJSON(app.Consts)
	rec := Record{
		Host:     host,
		App:      app,
		Level:    "error",
		Message:  "tab\there newline\n and control" + "\x01",
		LineVals: []string{`miss "quoted"`, "247"},
	}

	var m map[string]any
	if err := json.Unmarshal(AppendJSON(nil, rec, ts), &m); err != nil {
		t.Fatalf("AppendJSON produced invalid JSON: %v", err)
	}

	// Escaping round-trips: the decoded strings equal the originals.
	for key, want := range map[string]string{
		"host":         rec.Host.Name,
		"app":          rec.App.Name,
		"level":        rec.Level,
		"message":      rec.Message,
		"zone":         "us-east1-b",
		"namespace":    `has "quote"`,
		"cache_status": `miss "quoted"`,
	} {
		if got := m[key]; got != want {
			t.Errorf("%s mismatch: got %q; want %q", key, got, want)
		}
	}
	if got := m["timestamp"]; got != ts.Format(time.RFC3339Nano) {
		t.Errorf("timestamp mismatch: got %q; want %q", got, ts.Format(time.RFC3339Nano))
	}
	// Numeric fields decode as JSON numbers.
	if got := m["replicas"]; got != float64(3) {
		t.Errorf("replicas mismatch: got %v; want 3", got)
	}
	if got := m["duration_ms"]; got != float64(247) {
		t.Errorf("duration_ms mismatch: got %v; want 247", got)
	}
}

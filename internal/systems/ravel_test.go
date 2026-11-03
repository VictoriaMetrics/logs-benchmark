package systems

import (
	"testing"
	"time"
)

func TestRavelStoredRows(t *testing.T) {
	n, err := ravelStoredRows([]byte(`{"status":"success","data":{"columns":[{"name":"n","type":"Int64"}],"rows":[[12345]]},"stats":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if *n != 12345 {
		t.Fatalf("got %d, want 12345", *n)
	}
	for _, body := range []string{
		`{"status":"error","error":"boom"}`,
		`{"status":"success","data":{"rows":[]}}`,
		`{"status":"success","data":{"rows":[[1,2]]}}`,
		`not json`,
	} {
		if _, err := ravelStoredRows([]byte(body)); err == nil {
			t.Errorf("ravelStoredRows(%s): want an error", body)
		}
	}
}

func TestRavelHeader(t *testing.T) {
	strict := (&ravel{}).header()
	if got := strict.Get("Authorization"); got != "Bearer bench" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := strict.Get("x-ravel-ingest-mode"); got != "" {
		t.Fatalf("strict client sent x-ravel-ingest-mode %q", got)
	}
	if got := (&ravel{ingestMode: "buffered"}).header().Get("x-ravel-ingest-mode"); got != "buffered" {
		t.Fatalf("buffered client sent x-ravel-ingest-mode %q", got)
	}
}

func TestRavelTsFilter(t *testing.T) {
	start := time.Unix(0, 1_000_000_000).UTC()
	end := time.Unix(0, 2_000_000_001).UTC()
	want := "ts >= to_timestamp_nanos(1000000000) AND ts < to_timestamp_nanos(2000000001)"
	if got := ravelTsFilter(start, end); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Package querier measures every system under test concurrently. For each system
// it runs a query loop (per-query latency) and a stored-rows loop.
// Query loop can be disabled by setting Config.Interval=0.
//
// Query rounds run one at a time and measuring starts at the first clean round,
// so start-up emptiness is not recorded. The stored-row gauge is published only
// after the first successful sample and holds its last value on a later error.
package querier

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/metrics"

	"github.com/VictoriaMetrics/logs-benchmark/internal/systems"
)

// storedRowsInterval is how often the stored-rows loop samples.
const storedRowsInterval = 15 * time.Second

// Target is one named system to measure
type Target struct {
	Name   string
	Client systems.QueryClient
}

// Config holds the settings shared by every querier target.
type Config struct {
	Targets []Target

	// stream-scoped queries rotate through these, one per round, so repeated rounds do not hit a result cache
	Apps []string

	// time between the starts of two query rounds; <=0 disables the query loop
	Interval time.Duration

	// per-query and per-stored-rows-sample timeout
	Timeout time.Duration
}

func (cfg Config) validate() error {
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("at least one system must be set")
	}
	if len(cfg.Apps) == 0 {
		return fmt.Errorf("apps must not be empty")
	}
	if cfg.Timeout <= 0 {
		return fmt.Errorf("timeout must be > 0 (got %s)", cfg.Timeout)
	}
	return nil
}

// Run measures every target concurrently until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for _, t := range cfg.Targets {
		wg.Go(func() { runTarget(ctx, cfg, t) })
	}
	wg.Wait()
	return nil
}

// runTarget waits for one system's readiness, then runs its stored-rows loop and its query loop if enabled
func runTarget(ctx context.Context, cfg Config, t Target) {
	if !awaitReady(ctx, t) {
		return
	}
	var wg sync.WaitGroup
	wg.Go(func() { runStoredRows(ctx, cfg, t) })
	if cfg.Interval > 0 {
		wg.Go(func() { runQueries(ctx, cfg, t) })
	} else {
		logger.Infof("querier %s: query loop disabled (interval<=0); running stored-rows only", t.Name)
	}
	wg.Wait()
}

// awaitReady retries WaitReady until it succeeds or ctx is cancelled, so a system
// that is slow to start is still measured once it comes up.
func awaitReady(ctx context.Context, t Target) bool {
	for {
		logger.Infof("waiting for %s readiness...", t.Name)
		err := t.Client.WaitReady()
		if err == nil {
			return true
		}
		logger.Errorf("querier %s: not ready, retrying: %s", t.Name, err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Second):
		}
	}
}

// runQueries executes rounds of one target's query set, recording latency and errors.
func runQueries(ctx context.Context, cfg Config, t Target) {
	name := t.Name
	// The query set has the same shape for any App - only the stream-scoped
	// query embeds it - so build the metrics once, then rebuild the set each
	// round with a rotating App (see round).
	base := t.Client.Queries(systems.QueryParams{App: cfg.Apps[0]})
	durations := make([]*metrics.Histogram, len(base))
	errCounts := make([]*metrics.Counter, len(base))
	for i, q := range base {
		durations[i] = metrics.GetOrCreateHistogram(fmt.Sprintf(`logbench_query_duration_seconds{system=%q,query=%q}`, name, q.Name))
		errCounts[i] = metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_query_errors_total{system=%q,query=%q}`, name, q.Name))
	}

	logger.Infof("querying %s: %d queries per round, one round every %s", name, len(base), cfg.Interval)
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// round runs the query set once for the given app, recording samples and
	// errors only when record is true; returns true if every query passed
	round := func(record bool, app string) bool {
		queries := t.Client.Queries(systems.QueryParams{App: app})
		clean := true
		for i, q := range queries {
			now := time.Now()
			qctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
			t0 := time.Now()
			body, err := q.Do(qctx, now.Add(-q.Window), now)
			seconds := time.Since(t0).Seconds()
			cancel()
			if err == nil {
				err = q.Check(body)
			}
			if err != nil {
				if ctx.Err() != nil {
					return false
				}
				clean = false
				if record {
					errCounts[i].Inc()
					logger.Errorf("query %s on %s: %s", q.Name, name, err)
				} else {
					logger.Infof("warm-up query %s on %s: %s", q.Name, name, err)
				}
				continue
			}
			if record {
				durations[i].Update(seconds)
			}
		}
		return clean
	}
	wait := func() bool {
		select {
		case <-ticker.C:
			return true
		case <-ctx.Done():
			return false
		}
	}

	roundN := 0
	nextApp := func() string {
		app := cfg.Apps[roundN%len(cfg.Apps)]
		roundN++
		return app
	}

	// warm-up: measure only from the first clean round
	for !round(false, nextApp()) {
		if !wait() {
			return
		}
	}
	logger.Infof("%s warm-up complete; measuring", name)
	for {
		if !wait() {
			return
		}
		round(true, nextApp())
	}
}

// runStoredRows samples one target's stored-row count on a fixed interval and
// publishes it. The gauge is registered only after the first successful sample
// (so it is never a spurious zero) and its callback reads the last good value; a
// failing sample increments the error counter and leaves the last value in
// place. A system that reports no count keeps no loop.
func runStoredRows(ctx context.Context, cfg Config, t Target) {
	name := t.Name
	errs := metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_stored_rows_errors_total{system=%q}`, name))
	var rows atomic.Int64
	registered := false

	// sample reads one snapshot and returns whether polling should continue: it
	// stops on a cancelled context or once the system cleanly reports no count.
	sample := func() (keepGoing bool) {
		sctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		count, err := t.Client.StoredRows(sctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			errs.Inc()
			logger.Errorf("stored rows on %s: %s", name, err)
			return true
		}
		if count == nil {
			logger.Infof("querier %s: no stored count reported; stored-rows loop disabled", name)
			return false
		}
		rows.Store(*count)
		if !registered {
			metrics.GetOrCreateGauge(fmt.Sprintf(`logbench_stored_rows{system=%q}`, name), func() float64 {
				return float64(rows.Load())
			})
			registered = true
			logger.Infof("%s stored rows = %d (first sample)", name, *count)
		}
		return true
	}

	if !sample() {
		return
	}
	ticker := time.NewTicker(storedRowsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !sample() {
				return
			}
		}
	}
}

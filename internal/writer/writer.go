// Package writer fans one generated record stream out to every system under
// test: a pool of generators produces records, and each system drains its own
// buffered channel, encodes, and sends. On a full channel the writer drops
// (ingest test: each system finds its own ceiling) or blocks (query test: every
// system keeps identical data).
package writer

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/metrics"

	"github.com/VictoriaMetrics/logs-benchmark/internal/source/synthetic"
	"github.com/VictoriaMetrics/logs-benchmark/internal/systems"
)

// RecordSource produces the next record to fan out.
type RecordSource interface {
	Next() synthetic.Record
}

// Target is one named fan-out destination: a free-form instance name plus its
// write client.
type Target struct {
	Name   string
	Client systems.WriteClient
}

// Config holds the settings for one fan-out writer.
type Config struct {
	Targets    []Target       // fan-out targets
	Generators []RecordSource // record producers; each paces at Rate/len
	Rate       int            // target lines/s (the ramp's start rate when ramping)
	Workers    int            // send workers per system
	Batch      int            // lines per ingestion request
	Buffer     int            // per-system channel capacity
	DropOnFull bool           // drop on a full channel (ingest) vs block (query)

	// When RampStep > 0 the target rate starts at Rate and
	// rises by RampStep every RampInterval, up to RampMax (0 = uncapped).
	RampStep     int
	RampInterval time.Duration
	RampMax      int

	// RampHold: generate this long after the ramp hits RampMax, then stop. 0 = until cancelled.
	RampHold time.Duration
}

func (cfg Config) validate() error {
	if len(cfg.Targets) == 0 {
		return fmt.Errorf("at least one system must be set")
	}
	if len(cfg.Generators) == 0 {
		return fmt.Errorf("at least one generator must be set")
	}
	if cfg.Rate <= 0 {
		return fmt.Errorf("rate must be > 0 (got %d)", cfg.Rate)
	}
	if cfg.Workers <= 0 {
		return fmt.Errorf("workers must be > 0 (got %d)", cfg.Workers)
	}
	if cfg.Batch <= 0 {
		return fmt.Errorf("batch must be > 0 (got %d)", cfg.Batch)
	}
	if cfg.Buffer <= 0 {
		return fmt.Errorf("buffer must be > 0 (got %d)", cfg.Buffer)
	}
	if cfg.RampStep < 0 {
		return fmt.Errorf("rampStep cannot be negative (got %d)", cfg.RampStep)
	}
	if cfg.RampStep > 0 && cfg.RampInterval <= 0 {
		return fmt.Errorf("rampInterval must be > 0 when ramping (got %s)", cfg.RampInterval)
	}
	if cfg.RampMax < 0 {
		return fmt.Errorf("rampMax cannot be negative (got %d)", cfg.RampMax)
	}
	if cfg.RampStep > 0 && cfg.RampMax > 0 && cfg.RampMax < cfg.Rate {
		return fmt.Errorf("rampMax (%d) must be >= rate (%d) when set", cfg.RampMax, cfg.Rate)
	}
	if cfg.RampHold < 0 {
		return fmt.Errorf("rampHold cannot be negative (got %s)", cfg.RampHold)
	}
	if cfg.RampHold > 0 && (cfg.RampStep <= 0 || cfg.RampMax <= 0) {
		return fmt.Errorf("rampHold needs a capped ramp: set rampStep>0 and rampMax>0 (got rampHold=%s rampStep=%d rampMax=%d)", cfg.RampHold, cfg.RampStep, cfg.RampMax)
	}
	return nil
}

// targetRate returns the target lines/s at the given elapsed time
func (cfg Config) targetRate(elapsed time.Duration) int {
	if cfg.RampStep <= 0 {
		return cfg.Rate
	}
	rate := cfg.Rate + int(elapsed/cfg.RampInterval)*cfg.RampStep
	if cfg.RampMax > 0 && rate > cfg.RampMax {
		return cfg.RampMax
	}
	return rate
}

// stopAfter returns how long after start the producers keep generating
func (cfg Config) stopAfter() time.Duration {
	sa := time.Duration(math.MaxInt64)

	if cfg.RampHold > 0 {
		steps := (cfg.RampMax - cfg.Rate + cfg.RampStep - 1) / cfg.RampStep // ceil
		reachMax := time.Duration(steps) * cfg.RampInterval
		sa = reachMax + cfg.RampHold
	}

	return sa
}

// Run generates records and fans them out to every system until ctx is cancelled.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	if err := waitReady(cfg.Targets); err != nil {
		return err
	}

	chans := make([]chan systems.LogEntry, len(cfg.Targets))
	for i := range chans {
		chans[i] = make(chan systems.LogEntry, cfg.Buffer)
	}

	var consumerWg sync.WaitGroup
	for i, t := range cfg.Targets {
		consumerWg.Go(func() { cfg.consume(ctx, t, chans[i]) })
	}

	t0 := time.Now()
	n := len(cfg.Generators)
	metrics.GetOrCreateGauge(`logbench_target_rate`, func() float64 {
		return float64(cfg.targetRate(time.Since(t0)))
	})
	linesGenerated := metrics.GetOrCreateCounter(`logbench_lines_generated_total`)
	dropped := make([]*metrics.Counter, len(cfg.Targets))
	for i, t := range cfg.Targets {
		dropped[i] = metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_lines_dropped_total{system=%q}`, t.Name))
	}

	logger.Infof("fan-out to %d systems: target %d lines/s, %d generators, %d workers/system, buffer %d, dropOnFull=%v",
		len(cfg.Targets), cfg.Rate, n, cfg.Workers, cfg.Buffer, cfg.DropOnFull)
	if cfg.RampStep > 0 {
		logger.Infof("ramp: +%d lines/s every %s up to %d, hold %s", cfg.RampStep, cfg.RampInterval, cfg.RampMax, cfg.RampHold)
	}

	var genWg sync.WaitGroup
	for _, gen := range cfg.Generators {
		genWg.Go(func() { cfg.produce(ctx, gen, t0, chans, linesGenerated, dropped) })
	}
	genWg.Wait()
	for _, ch := range chans {
		close(ch)
	}
	consumerWg.Wait()
	return nil
}

func waitReady(targets []Target) error {
	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	for i, t := range targets {
		wg.Go(func() {
			logger.Infof("waiting for %s readiness...", t.Name)
			errs[i] = t.Client.WaitReady()
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("%s not ready: %w", targets[i].Name, err)
		}
	}
	return nil
}

// produce runs one generator, pacing at its share (target/n) of the rate. Every
// tick it works out how many records are due (rate x time since the last tick),
// generates them, and fans each out to every system. Each record gets a distinct
// timestamp, spread across the elapsed window.
func (cfg Config) produce(ctx context.Context, gen RecordSource, t0 time.Time, chans []chan systems.LogEntry, generated *metrics.Counter, dropped []*metrics.Counter) {
	n := len(cfg.Generators)
	stopAt := cfg.stopAfter()
	const tick = 100 * time.Millisecond
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	lastTick := t0
	lastNs := t0.UTC().UnixNano()
	var carry float64
	for {
		select {
		case now := <-ticker.C:
			// elapsed since start drives the ramp rate and the stop check
			elapsed := now.Sub(t0)
			if elapsed >= stopAt {
				return
			}
			rate := cfg.targetRate(elapsed) / n
			dt := now.Sub(lastTick)
			lastTick = now
			if dt <= 0 {
				continue
			}
			carry += float64(rate) * dt.Seconds()
			due := int(carry)
			carry -= float64(due)
			if due > rate { // cap at ~1s of lines so a stall does not burst afterwards
				due = rate
				carry = 0
			}
			if due <= 0 {
				continue
			}
			// Spread the timestamps over the elapsed window
			window := min(dt, time.Second)
			chunkStart := now.UTC().Add(-window)
			// Clamp chunkStart so timestamps stay strictly increasing even if
			// the clock steps back (e.g. NTP).
			if minNs := lastNs + 1; chunkStart.UnixNano() < minNs {
				chunkStart = time.Unix(0, minNs).UTC()
			}
			gap := window / time.Duration(due)
			for k := 0; k < due; k++ {
				ts := chunkStart.Add(time.Duration(k) * gap)
				lastNs = ts.UnixNano()
				it := systems.LogEntry{Rec: gen.Next(), Ts: ts}
				for s, ch := range chans {
					if cfg.DropOnFull {
						select {
						case ch <- it:
						default:
							dropped[s].Inc()
						}
					} else {
						select {
						case ch <- it:
						case <-ctx.Done():
							return
						}
					}
				}
			}
			generated.Add(due)
		case <-ctx.Done():
			return
		}
	}
}

// consume drains one system's channel with Workers goroutines, each encoding
// records into batches and sending them. When the system saturates, its senders
// block, the channel fills, and produce drops (or blocks) for it.
func (cfg Config) consume(ctx context.Context, t Target, ch <-chan systems.LogEntry) {
	name := t.Name
	sys := t.Client
	linesSent := metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_lines_sent_total{system=%q}`, name))
	bytesSent := metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_bytes_sent_total{system=%q}`, name))
	lineErrors := metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_send_line_errors_total{system=%q}`, name))
	sendRetries := metrics.GetOrCreateCounter(fmt.Sprintf(`logbench_send_retries_total{system=%q}`, name))
	metrics.GetOrCreateGauge(fmt.Sprintf(`logbench_queue_length{system=%q}`, name), func() float64 {
		return float64(len(ch))
	})

	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Go(func() {
			entries := make([]systems.LogEntry, 0, cfg.Batch)
			flush := func() {
				if len(entries) == 0 {
					return
				}
				b, err := sys.PrepareBatch(entries)
				n := len(entries)
				entries = entries[:0] // reuse; PrepareBatch copies into the batch body
				if err != nil {
					lineErrors.Add(n)
					logger.Errorf("cannot prepare a batch for %s: %s", name, err)
					return
				}
				retries, err := sys.Send(ctx, b)
				sendRetries.Add(retries)
				if err != nil {
					lineErrors.Add(b.Lines)
					logger.Errorf("cannot send a batch to %s: %s", name, err)
					return
				}
				linesSent.Add(b.Lines)
				bytesSent.Add(len(b.Body))
			}
			for it := range ch {
				entries = append(entries, it)
				if len(entries) >= cfg.Batch {
					flush()
				}
			}
			flush()
		})
	}
	wg.Wait()
}

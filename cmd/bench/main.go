// Command bench is the logs-benchmark harness: one process runs
// one role against the systems under test and exports its counters at /metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/buildinfo"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/cgroup"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/envflag"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/httpserver"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/procutil"

	"github.com/VictoriaMetrics/logs-benchmark/internal/querier"
	"github.com/VictoriaMetrics/logs-benchmark/internal/source/synthetic"
	"github.com/VictoriaMetrics/logs-benchmark/internal/systems"
	"github.com/VictoriaMetrics/logs-benchmark/internal/writer"
)

var (
	role = flag.String("role", "writer", "Process role. Supported values: writer, querier")
	// Systems under test as three position-paired arrays: -systemName[i],
	// -systemType[i] and -systemAddr[i] describe one instance.
	systemName     = flagutil.NewArrayString("systemName", "Instance name of a system under test (the 'system' metric label). Repeatable or comma-separated; position i pairs with -systemType and -systemAddr at i. Names must be unique.")
	systemType     = flagutil.NewArrayString("systemType", "Engine type selecting the client adapter: victorialogs, victorialogs-loki, elasticsearch, opensearch, loki, loki-json, signoz, clickstack. Paired by position with -systemName.")
	systemAddr     = flagutil.NewArrayString("systemAddr", "Base URL of the system at the same position in -systemName. Writer: the ingest endpoint; querier: the read endpoint.")
	rate           = flag.Int("rate", 5000, "Target ingestion rate in lines per second (the ramp's start rate when -rampStep is set)")
	rampStep       = flag.Int("rampStep", 0, "Ingest test: raise the target rate by this many lines/s every -rampInterval; 0 keeps -rate fixed")
	rampInterval   = flag.Duration("rampInterval", 5*time.Minute, "Ingest test: how long to hold each ramp step")
	rampMax        = flag.Int("rampMax", 0, "Ingest test: stop raising the target rate at this many lines/s; 0 means no cap")
	rampHold       = flag.Duration("rampHold", 0, "Ingest test: after the target rate reaches -rampMax, keep ingesting for this long, then stop generating and hold idle; 0 runs until stopped")
	workers        = flag.Int("workers", 0, "Send workers per system, i.e. in-flight requests per system; each encodes a batch (CPU) then blocks on the system's ack (I/O). 0 means 2x the available CPUs, enough in-flight requests to hide ack latency")
	batchLines     = flag.Int("batchLines", 2000, "The number of lines per ingestion request")
	generators     = flag.Int("generators", 0, "Parallel generator goroutines feeding the fan-out; generation is CPU-bound, so 0 means the number of available CPUs")
	buffer         = flag.Int("buffer", 8192, "Per-system fan-out channel capacity")
	dropOnFull     = flag.Bool("dropOnFull", false, "Drop on a full per-system channel (ingest) instead of blocking (query)")
	hosts          = flag.Int("hosts", 8, "Host cardinality; the number of log streams equals -hosts multiplied by -apps")
	apps           = flag.Int("apps", 4, "App cardinality; the number of log streams equals -hosts multiplied by -apps")
	seed           = flag.Int64("seed", 1, "Generator seed for reproducible records")
	queryInterval  = flag.Duration("queryInterval", time.Minute, "How frequently the querier starts a round of the query set; 0 disables the query loop (the stored-rows loop still runs)")
	queryTimeout   = flag.Duration("queryTimeout", 30*time.Second, "Per-query and per-stored-rows-sample timeout; a timed-out query counts as an error")
	httpListenAddr = flag.String("httpListenAddr", ":8080", "TCP address to listen for incoming http requests, including /metrics")
)

func main() {
	// Write flags and help message to stdout, since it is easier to grep or pipe.
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = usage
	envflag.Parse()
	buildinfo.Init()
	logger.Init()

	if *hosts <= 0 {
		logger.Fatalf("-hosts must be positive; got %d", *hosts)
	}
	if *apps <= 0 {
		logger.Fatalf("-apps must be positive; got %d", *apps)
	}
	if *workers < 0 {
		logger.Fatalf("-workers cannot be negative; got %d", *workers)
	}
	if *generators < 0 {
		logger.Fatalf("-generators cannot be negative; got %d", *generators)
	}
	if *workers == 0 {
		*workers = 2 * cgroup.AvailableCPUs()
	}
	if *generators == 0 {
		*generators = cgroup.AvailableCPUs()
	}

	logger.Infof("starting bench role=%q for %d system(s): %v", *role, len(*systemName), []string(*systemName))
	go httpserver.Serve([]string{*httpListenAddr}, requestHandler, httpserver.ServeOptions{})

	// Cancel ctx on SIGINT/SIGTERM to stop the role. The writer aborts its
	// in-flight sends and counts their lines as send errors.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sig := procutil.WaitForSigterm()
		logger.Infof("received signal %s; shutting down...", sig)
		cancel()
	}()

	insts, err := systemInstances()
	if err != nil {
		logger.Fatalf("cannot parse systems: %s", err)
	}

	switch *role {
	case "writer":
		targets := make([]writer.Target, len(insts))
		for i, in := range insts {
			c, err := systems.NewWriteClient(in.typ, in.addr)
			if err != nil {
				logger.Fatalf("system %q: %s", in.name, err)
			}
			targets[i] = writer.Target{Name: in.name, Client: c}
		}
		// One fleet shared by all generators; each generator has its own
		// seed for diverse content, and fan-out sends every record to every
		// system, so cardinality stays hosts x apps and all systems get the
		// same data.
		fleet := synthetic.NewFleet(*seed, *hosts, *apps)
		gens := make([]writer.RecordSource, *generators)
		for g := range gens {
			gens[g] = synthetic.New(fleet, *seed+int64(g))
		}
		err = writer.Run(ctx, writer.Config{
			Targets:      targets,
			Generators:   gens,
			Rate:         *rate,
			Workers:      *workers,
			Batch:        *batchLines,
			Buffer:       *buffer,
			DropOnFull:   *dropOnFull,
			RampStep:     *rampStep,
			RampInterval: *rampInterval,
			RampMax:      *rampMax,
			RampHold:     *rampHold,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Fatalf("writer: %s", err)
		}
		// Hold here after the run completes, still serving /metrics, until shutdown.
		// (a restarted process would re-run the ramp)
		if ctx.Err() == nil {
			logger.Infof("run complete; holding idle, still serving /metrics until shutdown")
			<-ctx.Done()
		}
	case "querier":
		targets := make([]querier.Target, len(insts))
		for i, in := range insts {
			c, err := systems.NewQueryClient(in.typ, in.addr)
			if err != nil {
				logger.Fatalf("system %q: %s", in.name, err)
			}
			targets[i] = querier.Target{Name: in.name, Client: c}
		}
		// the writers' seed reproduces the app names, so the queried streams exist
		appNames := synthetic.NewFleet(*seed, *hosts, *apps).AppNames()
		err = querier.Run(ctx, querier.Config{
			Targets:  targets,
			Apps:     appNames,
			Interval: *queryInterval,
			Timeout:  *queryTimeout,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Fatalf("querier: %s", err)
		}
	default:
		logger.Fatalf("unexpected -role=%q; supported values: writer, querier", *role)
	}

	if err := httpserver.Stop([]string{*httpListenAddr}); err != nil {
		logger.Fatalf("cannot stop the webservice: %s", err)
	}
	logger.Infof("bench has been stopped")
}

// httpserver already serves /metrics, /health and /debug/pprof; nothing to add.
func requestHandler(_ http.ResponseWriter, _ *http.Request) bool {
	return false
}

// instance is one system under test, parsed from the position-paired flags.
type instance struct {
	name, typ, addr string
}

// systemInstances pairs the -systemName/-systemType/-systemAddr arrays by
// position, validating that each instance has all three fields and a unique name.
func systemInstances() ([]instance, error) {
	names := *systemName
	if len(names) == 0 {
		return nil, fmt.Errorf("no -systemName given")
	}
	seen := make(map[string]bool, len(names))
	insts := make([]instance, len(names))
	for i, name := range names {
		typ := systemType.GetOptionalArg(i)
		addr := systemAddr.GetOptionalArg(i)
		if name == "" || typ == "" || addr == "" {
			return nil, fmt.Errorf("system %d: -systemName/-systemType/-systemAddr must all be set (got name=%q type=%q addr=%q)", i, name, typ, addr)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate -systemName %q; names must be unique", name)
		}
		seen[name] = true
		insts[i] = instance{name: name, typ: typ, addr: addr}
	}
	return insts, nil
}

func usage() {
	const s = `
bench runs one role of the logs-benchmark harness against the systems under test.

See the README at https://github.com/VictoriaMetrics/logs-benchmark .
`
	flagutil.Usage(s)
}

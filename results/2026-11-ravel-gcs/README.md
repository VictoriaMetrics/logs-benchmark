# Ingestion test, Ravel on GCS and VictoriaLogs (November 2026)

Results of the ravel-gcs ingestion test, which follows the method of
benchmark 1 in the [roadmap](../../README.md#roadmap) with two engines. The
runbook is in the [README](../../deploy/benchmarks/ravel-gcs/README.md), and the
run config is [ingest-ramp.yaml](../../deploy/benchmarks/ravel-gcs/ingest-ramp.yaml).

## Setup

Each engine runs on its own n2d-standard-8 node (8 vCPUs, 32 GB of RAM) on GKE
`<TBD>`. VictoriaLogs stores on a 500 GiB pd-balanced disk on its node. Ravel
stores in a GCS bucket and uses no disk.

| Engine       | Version          | Install                          |
| ------------ | ---------------- | -------------------------------- |
| VictoriaLogs | `<TBD>`          | victoria-logs-single `<TBD>`     |
| Ravel        | `<RAVEL_VERSION>` | ravel-render.sh, flags in the runbook |

Ravel runs with buffered acks (`x-ravel-ingest-mode: buffered`): a 2xx means
the lines are buffered, not yet committed to GCS. Stored rows count only
committed rows.

## How the numbers are computed

- The load is a ramp from 10k to 180k lines/s in steps of 10k lines/s every
  10 minutes. The writer uses 32 workers and requests of 2,000 lines, and it
  drops the lines that an engine cannot take (`dropOnFull`), so each engine
  plateaus at its own ceiling.
- The stored rate is `sum by(system)(clamp_min(deriv(logbench_stored_rows[3m]),0))`,
  sampled once a minute.
- **Maximum ingestion rate**: the best 30-minute window of that stored rate
  over the load.
- **CPU** `<TBD: basis>`, **memory** `<TBD: basis>`.
- **Disk space**: for VictoriaLogs the node disk after the ramp. For Ravel the
  bucket's bytes, `gcloud storage du -s gs://<bucket>`, divided by its stored
  rows.
- Number of runs: `<TBD>`.

## Results

### Maximum ingestion rate (lines/s)

| Engine       | Rate    | Range over runs |
| ------------ | ------- | --------------- |
| VictoriaLogs | `<TBD>` | `<TBD>`         |
| Ravel        | `<TBD>` | `<TBD>`         |

### CPU (cores)

| Engine       | 10k     | 20k     | 40k     | 80k     | 120k    | 150k    |
| ------------ | ------- | ------- | ------- | ------- | ------- | ------- |
| VictoriaLogs | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Ravel        | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

### Memory without the page cache (GiB)

| Engine       | 10k     | 20k     | 40k     | 80k     | 120k    | 150k    |
| ------------ | ------- | ------- | ------- | ------- | ------- | ------- |
| VictoriaLogs | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Ravel        | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

### Disk space

| Engine       | MiB per million lines | Compression ratio |
| ------------ | --------------------- | ----------------- |
| VictoriaLogs | `<TBD>`               | `<TBD>`           |
| Ravel        | `<TBD>`               | `<TBD>`           |

## Charts

`<TBD>`

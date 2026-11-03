# Ingestion test, Ravel on GCS and VictoriaLogs (November 2026)

Results of the ravel-gcs ingestion test, which follows the method of
benchmark 1 in the [roadmap](../../README.md#roadmap) with two engines. The
method is in the [README](../../README.md) and the
[runbook](../../deploy/benchmarks/ravel-gcs/README.md). The run config is
[ingest-ramp.yaml](../../deploy/benchmarks/ravel-gcs/ingest-ramp.yaml).

## Setup

Each engine runs on its own n2d-standard-8 node (8 vCPUs, 32 GB of RAM) on GKE
`<TBD>`. VictoriaLogs stores on a 500 GiB pd-balanced disk on its node. Ravel
stores in a GCS bucket and uses no disk.

| Engine       | Version           | Install                               |
| ------------ | ----------------- | ------------------------------------- |
| VictoriaLogs | `<TBD>`         | victoria-logs-single `<TBD>`        |
| Ravel        | `<RAVEL_VERSION>` | ravel-render.sh, flags in the runbook |

Ravel runs at its shipped defaults with the flags listed in the runbook, and
its writer asks for buffered acks (`x-ravel-ingest-mode: buffered`): a 2xx
means the lines are buffered, not yet committed to GCS. Stored rows count only
committed rows.

## How the numbers are computed

- Each number is the median of three runs.
- **Maximum ingestion rate**: the highest average stored rate over one
  10-minute step, among the steps before the engine's first crash.
- **N/A**: in at least one run, the engine stored less than 90% of the
  step's target.
- **CPU** is the step average and **memory** the step peak without the page
  cache.
- **Disk writes per line**: for VictoriaLogs, the step average of the bytes
  written to its disk divided by the stored rate. Ravel has no disk, so two
  figures replace it, each the step average divided by the stored rate: **GCS
  bytes written per line** and **GCS write requests per line**. They come from
  ravel-server's own object-store counters, `ravel_store_bytes_total` and
  `ravel_store_calls_total` with `op="put"`, which the pod scrape
  (`deploy/monitoring/ravel-podscrape.yaml`) collects. The bytes are the
  bytes offered by the put calls, not wire bytes, and the calls are counted by
  ravel-server, not by GCS.
- **Disk space**: for VictoriaLogs, read after the ramp, once its disk has
  stayed the same for 10 minutes. For Ravel, the bucket's object bytes after
  the ramp, once they have stayed the same for 10 minutes
  (`gcloud storage du -s gs://<bucket>`). The compression ratio is the raw
  JSON size (887 MiB per million lines) divided by it.
- Each engine owns its node, so CPU, memory and disk count every pod on it.

The VictoriaLogs figures from this cluster can be compared with the 120k of
[benchmark 1](../2026-10-ingest/README.md), which checks that the cluster
reproduces the published one.

## Results

### Maximum ingestion rate (lines/s)

| Engine       | Median  | Range over 3 runs |
| ------------ | ------- | ----------------- |
| VictoriaLogs | `<TBD>` | `<TBD>`         |
| Ravel        | `<TBD>` | `<TBD>`         |

### CPU (cores)

| Engine       | 10k | 20k | 40k | 80k | 120k | 150k |
| ------------ | --- | --- | --- | --- | --- | --- |
| VictoriaLogs | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Ravel        | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

### Memory without the page cache (GiB)

| Engine       | 10k | 20k | 40k | 80k | 120k | 150k |
| ------------ | --- | --- | --- | --- | --- | --- |
| VictoriaLogs | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| Ravel        | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

### Disk space

| Engine       | MiB per million lines | Compression ratio |
| ------------ | --------------------- | ----------------- |
| VictoriaLogs | `<TBD>`               | `<TBD>`         |
| Ravel        | `<TBD>`               | `<TBD>`         |

### Disk writes (bytes per stored line)

VictoriaLogs only. These numbers include the writes of indexes and background
merges.

| Engine       | 10k | 20k | 40k | 80k | 120k | 150k |
| ------------ | --- | --- | --- | --- | --- | --- |
| VictoriaLogs | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

### Ravel object-store writes per stored line

| Metric                   | 10k | 20k | 40k | 80k | 120k | 150k |
| ------------------------ | --- | --- | --- | --- | --- | --- |
| GCS bytes written        | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |
| GCS write requests       | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` | `<TBD>` |

## Charts

`<TBD>`

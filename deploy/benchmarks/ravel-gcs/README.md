# Benchmark: Ravel on GCS against VictoriaLogs

Ravel is a telemetry database that keeps no state on local disk: object
storage is its only durable store. Here that store is a GCS bucket, used
natively (`--store gcs`) and reached through GKE Workload Identity, so no key
exists. The VictoriaLogs instance is installed exactly as in `all-systems`,
on the same machine type, and its ceiling is the check that this cluster
reproduces the published one.

The Ravel instance is `ravel-buffered`: its writer asks for buffered acks
(`x-ravel-ingest-mode: buffered`), so a 2xx from `/v1/logs` means the lines
are buffered, before they are committed to GCS. Stored rows are counted with
a SQL `count(*)` over `logs`, which sees only committed rows, so the ceiling
counts durable rows, as for every other engine.

## Machines

- Harness: one n2d-standard-32, as in `all-systems`.
- One n2d-standard-8 per instance: `victorialogs` and `ravel-buffered`
  (two instance pools, 16 N2D vCPUs, next to the 32 of the harness).
- VictoriaLogs stores on its node's 500 GiB pd-balanced disk. Ravel stores in
  its GCS bucket and uses no volume.

## Ravel configuration

The run uses a released Ravel image (`<RAVEL_VERSION>`, passed as
`RAVEL_VERSION`) at its shipped defaults. The only flags set explicitly are
the ones the install passes:

```bash
RAVEL_FLAGS="--max-ingest-buffer-bytes 4294967296 --shards 2 \
  --max-flush-delay 100ms --max-flush-delay-idle 40s --min-flush-bytes 262144 \
  --catalog-record-cache-max-bytes-per-tenant 462000000 \
  --cpu-gate-write-permits 8"
```

| Flag | Value | Default | What it does |
|---|---|---|---|
| `--max-ingest-buffer-bytes` | 4 GiB | 512 MiB | Caps the ingest bytes held in memory across all tenants; a request past the cap is refused with a 429. |
| `--shards` | 2 | 4 | Sets the number of ingest shards in the process. |
| `--max-flush-delay` | 100ms | 2s | Sets how long a buffer that holds at least `--min-flush-bytes` waits before it is flushed to object storage. |
| `--max-flush-delay-idle` | 40s | 40s | Sets how long a buffer with less than `--min-flush-bytes` waits before it is flushed. |
| `--min-flush-bytes` | 262144 | 262144 | Sets the buffer size at which the fast `--max-flush-delay` clock applies instead of the idle one. |
| `--catalog-record-cache-max-bytes-per-tenant` | 462000000 | 45000000 | Sets the per-tenant memory budget for the catalog's commit-record and compaction-record caches. |
| `--cpu-gate-write-permits` | 8 | max(1, cores / 2), 4 on these nodes | Sets how many flush-encode and payload-decode jobs run at once. |

Ravel refuses `--max-flush-delay` set alone, so the three flush flags are
passed together; the two that equal their defaults are passed to satisfy
that rule.

`deploy/systems/ravel-render.sh` adds the flags that every instance needs:
`--store gcs --gcs-bucket <bucket> --tenant-hash-unkeyed --tenant-token
bench=bench`, the listener addresses, and `--limits-file` with
`ingest_bytes_per_sec = "unlimited"`. Ravel caps each tenant at 32 MiB/s of
ingest by default, about 30k lines/s of this OTLP load, and no other engine
here has a per-tenant cap.

## Run

```bash
export PROJECT=<project> ZONE=us-central1-a CLUSTER=logs-bench-ravel
export BENCH=deploy/benchmarks/ravel-gcs

deploy/benchmarks/ravel-gcs/gcp-up.sh $PROJECT $ZONE $CLUSTER \
  victorialogs ravel-buffered
export CONTEXT=gke_${PROJECT}_${ZONE}_${CLUSTER} HARNESS_IMAGE=<printed by gcp-up.sh>

make install-victorialogs POOL=victorialogs CONTEXT=$CONTEXT BENCH=$BENCH CONFIRMED=1
make install-ravel POOL=ravel-buffered CONTEXT=$CONTEXT CONFIRMED=1 \
  RAVEL_VERSION=<RAVEL_VERSION> RAVEL_FLAGS="$RAVEL_FLAGS"
make monitoring-up ravel-scrape-up CONTEXT=$CONTEXT CONFIRMED=1
make status CONTEXT=$CONTEXT

make deploy-bench CONTEXT=$CONTEXT BENCH=$BENCH RUN=ingest-ramp CONFIRMED=1 \
  HELM_SET="--set image.repository=${HARNESS_IMAGE%:*} --set image.tag=${HARNESS_IMAGE##*:}"
```

`gcp-up.sh` builds the harness image on Cloud Build with `cloudbuild.yaml`
and prints its name.

The run config `ingest-ramp.yaml` has the writer settings of
`all-systems/ingest.yaml`: 10k lines/s, plus 10k every 10 minutes, up to 180k.

## Read the results

The "Logs Benchmark" dashboard panels apply as in `all-systems`, except that
Ravel has no volume: its disk figures are the bucket's bytes once the run
ends, `gcloud storage du -s gs://<bucket>`, divided by its stored rows, and its
request counts come from ravel-server's own `/metrics`.

## Clean up

```bash
deploy/benchmarks/ravel-gcs/gcp-down.sh $PROJECT $ZONE $CLUSTER \
  victorialogs ravel-buffered
```

# Benchmark: all systems

This benchmark runs the six engines side by side, one instance each, on
identical machines. The published results come from it.

> [!WARNING]
> One ingest run uses seven machines and about 3 TiB of disk for about
> 5 hours. At on-demand prices in us-central1, this costs about 20 USD
> (October 2026). Delete the cluster when you finish.

This benchmark has two run configs, and each file explains its parameters
in comments:

- `ingest.yaml` finds the ingest ceiling of each instance. A run takes
  about 4 hours: 3 hours of load, and up to one more hour for the disks to
  settle before disk space can be read.
- `query.yaml` measures query performance of six systems. It is still in
  progress.

## Cluster

Create a GKE Standard cluster. Its default node pool runs the harness. Then
add one node pool for each instance, with one node in it:

```bash
export CLUSTER=<cluster name> ZONE=<zone>

# The default pool runs the harness: the writer, the querier and monitoring.
# Logging and monitoring agents are off, because they would also run on the
# measured nodes.
gcloud container clusters create $CLUSTER --zone $ZONE \
  --num-nodes 1 --machine-type n2d-standard-32 --min-cpu-platform "AMD Milan" \
  --logging=NONE --monitoring=NONE --no-enable-managed-prometheus

# One node for each instance. The taint keeps every other pod off it.
for s in victorialogs elasticsearch opensearch loki signoz clickstack; do
  gcloud container node-pools create $s --cluster $CLUSTER --zone $ZONE \
    --num-nodes 1 --machine-type n2d-standard-8 --min-cpu-platform "AMD Milan" \
    --node-labels logs-benchmark.victoriametrics.com/system=$s \
    --node-taints logs-benchmark.victoriametrics.com/system=$s:NoSchedule
done

# Block node upgrades for two days, because an upgrade restarts the nodes.
gcloud container clusters update $CLUSTER --zone $ZONE \
  --add-maintenance-exclusion-name bench \
  --add-maintenance-exclusion-end +P2D \
  --add-maintenance-exclusion-scope no_upgrades
```

The region needs quota for 80 N2D vCPUs and about 4 TB of SSD disk.

Client tools: make, kubectl, helm, and gcloud with `gke-gcloud-auth-plugin`.

## Install

Run all commands from the repo root. The instances and the harness go into
the `logs-bench` namespace. Each command asks before it changes the cluster,
and `CONFIRMED=1` skips the question. If you added the Helm repos of the
engines before, run `helm repo update` first.

```bash
export CONTEXT=gke_$(gcloud config get-value project)_${ZONE}_${CLUSTER}
export BENCH=deploy/benchmarks/all-systems

make install-all-systems CONTEXT=$CONTEXT BENCH=$BENCH
make monitoring-up       CONTEXT=$CONTEXT
make status              CONTEXT=$CONTEXT
```

Wait until every pod is Running. The setup jobs end as Completed. SigNoz and
ClickStack take a few minutes.

## Run

```bash
make deploy-bench CONTEXT=$CONTEXT BENCH=$BENCH RUN=ingest
```

The writer starts sending about 3 minutes after the deploy, when every
instance answers. `RUN=query` deploys `query.yaml` instead.

## Read the results

```bash
make grafana CONTEXT=$CONTEXT
```

This command forwards Grafana to `localhost:3000` and prints the password.
The results are on the "Logs Benchmark" dashboard.

### Ingestion run

Each number in the results comes from one panel:

| Number      | Panel                                                |
| ----------- | ---------------------------------------------------- |
| Ceiling     | Ingest rate: target vs stored (lines/s, server-side) |
| Crashes     | Container restarts per system (cumulative)           |
| CPU         | CPU by system (cores)                                |
| Memory      | Memory: anonymous per system                         |
| Disk writes | Disk write by system (bytes/s)                       |
| Disk space  | Disk per million log lines (bytes)                   |

The [README](../../../README.md#ingestion-test) explains how to read the
ceiling.

### Query run

In progress.

## Clean up

To stop the harness but keep the instances:

```bash
make undeploy-bench CONTEXT=$CONTEXT
```

Every run must start from empty instances. So between two runs, remove the
instances and their disks, and then install again. Read the results first,
because deleting the namespace also removes the dashboard until the next
`deploy-bench`.

```bash
make uninstall-all-systems CONTEXT=$CONTEXT
kubectl --context $CONTEXT delete namespace logs-bench
```

When you finish, remove everything in this order, so that GKE also deletes
the disks:

```bash
make undeploy-bench        CONTEXT=$CONTEXT
make uninstall-all-systems CONTEXT=$CONTEXT
kubectl --context $CONTEXT delete namespace logs-bench logs-bench-monitoring
kubectl --context $CONTEXT get pv    # wait until this lists nothing
gcloud container clusters delete $CLUSTER --zone $ZONE
gcloud compute disks list --filter="name~^pvc-"   # delete any disk listed here
```

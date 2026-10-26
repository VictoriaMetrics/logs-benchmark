# Benchmark: dev

The local dev loop, which measures nothing: one VictoriaLogs on a kind
cluster plus the bench built from the working tree, so writer, querier and
dashboard changes can be exercised end to end on a laptop.

- `query.yaml`: light fixed-rate run config.
- `systems/victorialogs.yaml`: the sizing for a laptop, a 10Gi PVC on kind's
  `standard` storage class.

kind's single node carries the label
`logs-benchmark.victoriametrics.com/system=victorialogs` (set in
`deploy/kind/kind.yml`), so the same pin files as the cloud benchmarks are
exercised. One node holds one value of that label key, so dev is single-system
by design.

Client tools: make, docker, kind, kubectl and helm.

```bash
make kind-up
make deploy-dev          # build + load image, install VictoriaLogs, deploy the bench
make monitoring-up-dev
make grafana
make undeploy-dev
make kind-down
```

The dev image tag is a source checksum: after a code change `make deploy-dev`
rolls the pods, with no change it is a no-op.

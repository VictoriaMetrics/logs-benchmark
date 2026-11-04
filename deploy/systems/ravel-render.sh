#!/usr/bin/env bash
# Renders one Ravel instance (Deployment + Service) on stdout, for
# `kubectl apply -f -`. Ravel has no helm chart here; this is the whole install.
#
# usage: ravel-render.sh POOL IMAGE BUCKET [ravel-server flag ...]
#
# POOL is the instance identity, as for every other engine: node pool label and
# taint, pod label, Service name, run-config `name`. Its object storage is the
# GCS bucket BUCKET (ravel-server --store gcs), reached with GKE
# Workload Identity through the Kubernetes service account "ravel": no key
# is stored anywhere. The Secret ravel-bench holds RAVEL_AUDIT_TOKEN_KEY.
# Every flag after IMAGE is appended to ravel-server's command line; each one
# that departs from Ravel's defaults is listed in the benchmark README.
set -euo pipefail

if [ $# -lt 3 ]; then
	echo "usage: $0 POOL IMAGE BUCKET [ravel-server flag ...]" >&2
	exit 64
fi
pool=$1
image=$2
bucket=$3
shift 3
if ! printf '%s' "$pool" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,51}[a-z0-9])?$'; then
	echo "POOL must be a lowercase RFC-1123 label (got '$pool')" >&2
	exit 64
fi

extra=""
for f in "$@"; do
	case "$f" in
	*\"* | *\\*)
		echo "flag must not contain quotes or backslashes: $f" >&2
		exit 64
		;;
	esac
	extra+="            - \"$f\""$'\n'
done

cat <<EOF
# Ravel's admission control caps each tenant at 32 MiB/s of ingest by
# default (see Ravel's admission limits guide). The benchmark runs one tenant per
# instance and no other engine has a per-tenant cap, so the cap is lifted.
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${pool}-limits
data:
  limits.toml: |
    [defaults]
    ingest_bytes_per_sec = "unlimited"
---
apiVersion: v1
kind: Service
metadata:
  name: ${pool}
  labels:
    app.kubernetes.io/name: ravel
    logs-benchmark.victoriametrics.com/system: ${pool}
spec:
  selector:
    app.kubernetes.io/name: ravel
    logs-benchmark.victoriametrics.com/system: ${pool}
  ports:
    - name: http
      port: 4318
      targetPort: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${pool}
  labels:
    app.kubernetes.io/name: ravel
    logs-benchmark.victoriametrics.com/system: ${pool}
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: ravel
      logs-benchmark.victoriametrics.com/system: ${pool}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ravel
        logs-benchmark.victoriametrics.com/system: ${pool}
    spec:
      serviceAccountName: ravel
      nodeSelector:
        logs-benchmark.victoriametrics.com/system: ${pool}
      tolerations:
        - key: logs-benchmark.victoriametrics.com/system
          operator: Equal
          value: ${pool}
          effect: NoSchedule
      # Ravel refuses to start on a bucket with no qualification record. The
      # qualify run probes the bucket's conditional-write and listing semantics
      # once and leaves an existing record of the same suite version alone.
      volumes:
        - name: limits
          configMap:
            name: ${pool}-limits
      initContainers:
        - name: qualify
          image: ${image}
          command: ["/usr/local/bin/ravel-cli", "--store", "gcs", "--gcs-bucket", "${bucket}", "store", "qualify"]
          envFrom:
            - secretRef:
                name: ravel-bench
      containers:
        - name: ravel
          image: ${image}
          args:
            - "--store"
            - "gcs"
            - "--gcs-bucket"
            - "${bucket}"
            - "--tenant-hash-unkeyed"
            - "--tenant-token"
            - "bench=bench"
            - "--listen-http"
            - "0.0.0.0:4318"
            - "--listen-grpc"
            - "0.0.0.0:4317"
            - "--listen-health"
            - "0.0.0.0:8081"
            - "--limits-file"
            - "/etc/ravel/limits.toml"
${extra}          envFrom:
            - secretRef:
                name: ravel-bench
          volumeMounts:
            - name: limits
              mountPath: /etc/ravel
              readOnly: true
          ports:
            - name: http
              containerPort: 4318
            - name: health
              containerPort: 8081
          readinessProbe:
            httpGet:
              path: /readyz
              port: health
            periodSeconds: 5
          livenessProbe:
            httpGet:
              path: /healthz
              port: health
            periodSeconds: 10
            failureThreshold: 6
          # No limits, so Ravel gets its whole node, like every other engine.
          resources:
            requests:
              cpu: 250m
              memory: 512Mi
EOF

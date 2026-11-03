#!/usr/bin/env bash
# Creates everything a ravel-gcs run needs in one GCP project: the GKE cluster
# with Workload Identity, its harness pool and one tainted pool per instance,
# one GCS bucket per Ravel instance that the pods reach as a Google service
# account (no keys), the Secret holding Ravel's audit token key, and the
# harness image (built by Cloud Build, so no local docker is needed).
#
# usage: gcp-up.sh PROJECT ZONE CLUSTER POOL [POOL ...]
#   POOL names a measured instance; victorialogs gets a node pool only, every
#   other POOL is a Ravel instance and also gets a bucket.
#
# Re-running skips what already exists. gcp-down.sh removes it all.
set -euo pipefail

if [ $# -lt 4 ]; then
	echo "usage: $0 PROJECT ZONE CLUSTER POOL [POOL ...]" >&2
	exit 64
fi
project=$1
zone=$2
cluster=$3
shift 3
pools=("$@")
region=${zone%-*}
ns=logs-bench
sa_name=logsbench-ravel
sa="${sa_name}@${project}.iam.gserviceaccount.com"
ar_repo=logs-bench
here=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$here/../../.." && pwd)
gc=(gcloud --project "$project" --quiet)

echo "== cluster $cluster in $zone"
if ! "${gc[@]}" container clusters describe "$cluster" --zone "$zone" >/dev/null 2>&1; then
	# Same harness pool as all-systems: logging and monitoring agents off,
	# because they would also run on the measured nodes.
	"${gc[@]}" container clusters create "$cluster" --zone "$zone" \
		--num-nodes 1 --machine-type n2d-standard-32 --min-cpu-platform "AMD Milan" \
		--logging=NONE --monitoring=NONE --no-enable-managed-prometheus \
		--workload-pool "${project}.svc.id.goog"
	"${gc[@]}" container clusters update "$cluster" --zone "$zone" \
		--add-maintenance-exclusion-name bench \
		--add-maintenance-exclusion-end "$(date -u -v+2d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '+2 days' +%Y-%m-%dT%H:%M:%SZ)" \
		--add-maintenance-exclusion-scope no_upgrades
fi

for p in "${pools[@]}"; do
	if ! "${gc[@]}" container node-pools describe "$p" --cluster "$cluster" --zone "$zone" >/dev/null 2>&1; then
		echo "== node pool $p"
		"${gc[@]}" container node-pools create "$p" --cluster "$cluster" --zone "$zone" \
			--num-nodes 1 --machine-type n2d-standard-8 --min-cpu-platform "AMD Milan" \
			--node-labels "logs-benchmark.victoriametrics.com/system=$p" \
			--node-taints "logs-benchmark.victoriametrics.com/system=$p:NoSchedule" \
			--workload-metadata GKE_METADATA
	fi
done

"${gc[@]}" container clusters get-credentials "$cluster" --zone "$zone"
context="gke_${project}_${zone}_${cluster}"
kc=(kubectl --context "$context")
"${kc[@]}" get namespace "$ns" >/dev/null 2>&1 || "${kc[@]}" create namespace "$ns"

if ! "${kc[@]}" -n "$ns" get secret ravel-bench >/dev/null 2>&1; then
	key=$(openssl rand -hex 32)
	"${kc[@]}" -n "$ns" create secret generic ravel-bench --from-literal=RAVEL_AUDIT_TOKEN_KEY="$key" >/dev/null
	unset key
fi

if ! "${gc[@]}" iam service-accounts describe "$sa" >/dev/null 2>&1; then
	"${gc[@]}" iam service-accounts create "$sa_name" --display-name "logs-benchmark Ravel buckets"
fi

for p in "${pools[@]}"; do
	[ "$p" = victorialogs ] && continue
	bucket="${project}-logsbench-${p}"
	echo "== bucket gs://$bucket"
	if ! "${gc[@]}" storage buckets describe "gs://$bucket" >/dev/null 2>&1; then
		# Single region next to the cluster. No soft delete, so teardown does
		# not leave a week of billed deleted objects.
		"${gc[@]}" storage buckets create "gs://$bucket" --location "$region" \
			--default-storage-class STANDARD --uniform-bucket-level-access \
			--public-access-prevention --soft-delete-duration 0
	fi
	"${gc[@]}" storage buckets add-iam-policy-binding "gs://$bucket" \
		--member "serviceAccount:$sa" --role roles/storage.objectAdmin >/dev/null
done

# Ravel pods run as the Kubernetes service account "ravel", which Workload
# Identity maps to the Google service account that holds the bucket grants.
"${kc[@]}" -n "$ns" get serviceaccount ravel >/dev/null 2>&1 || "${kc[@]}" -n "$ns" create serviceaccount ravel
"${kc[@]}" -n "$ns" annotate serviceaccount ravel --overwrite "iam.gke.io/gcp-service-account=$sa" >/dev/null
"${gc[@]}" iam service-accounts add-iam-policy-binding "$sa" \
	--role roles/iam.workloadIdentityUser \
	--member "serviceAccount:${project}.svc.id.goog[${ns}/ravel]" >/dev/null

echo "== harness image"
if ! "${gc[@]}" artifacts repositories describe "$ar_repo" --location "$region" >/dev/null 2>&1; then
	"${gc[@]}" artifacts repositories create "$ar_repo" --location "$region" --repository-format docker
fi
tag=$(cd "$repo_root" && git rev-parse --short=12 HEAD)
if [ -n "$(cd "$repo_root" && git status --porcelain -- cmd internal go.mod go.sum Dockerfile)" ]; then
	echo "commit the harness sources first: the image tag is the commit" >&2
	exit 1
fi
image="${region}-docker.pkg.dev/${project}/${ar_repo}/logs-bench"
if ! "${gc[@]}" artifacts docker images describe "${image}:${tag}" >/dev/null 2>&1; then
	"${gc[@]}" builds submit "$repo_root" --region "$region" \
		--config "$here/cloudbuild.yaml" --substitutions "_IMAGE=${image}:${tag}"
fi

echo
echo "CONTEXT=$context"
echo "HARNESS_IMAGE=${image}:${tag}"

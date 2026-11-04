#!/usr/bin/env bash
# Removes what gcp-up.sh created: the cluster (and with it the PVC disks once
# the namespaces are gone), the Ravel buckets with every object in them, the
# HMAC keys and the service account. The Artifact Registry repository is kept;
# delete it by hand when no run needs the harness image.
#
# usage: gcp-down.sh PROJECT ZONE CLUSTER POOL [POOL ...]
set -euo pipefail

if [ $# -lt 4 ]; then
	echo "usage: $0 PROJECT ZONE CLUSTER POOL [POOL ...]" >&2
	exit 64
fi
project=$1
zone=$2
cluster=$3
shift 3
sa="logsbench-ravel@${project}.iam.gserviceaccount.com"
gc=(gcloud --project "$project" --quiet)
context="gke_${project}_${zone}_${cluster}"

if "${gc[@]}" container clusters describe "$cluster" --zone "$zone" >/dev/null 2>&1; then
	# Namespaces first, so GKE deletes the PVC disks before the cluster goes.
	kubectl --context "$context" delete namespace logs-bench logs-bench-monitoring --ignore-not-found --wait=true || true
	"${gc[@]}" container clusters delete "$cluster" --zone "$zone"
fi

for p in "$@"; do
	[ "$p" = victorialogs ] && continue
	bucket="${project}-logsbench-${p}"
	if "${gc[@]}" storage buckets describe "gs://$bucket" >/dev/null 2>&1; then
		"${gc[@]}" storage rm --recursive "gs://$bucket"
	fi
done

if "${gc[@]}" iam service-accounts describe "$sa" >/dev/null 2>&1; then
	for id in $("${gc[@]}" storage hmac list --service-account "$sa" --format 'value(accessId)'); do
		"${gc[@]}" storage hmac update "$id" --deactivate >/dev/null || true
		"${gc[@]}" storage hmac delete "$id"
	done
	"${gc[@]}" iam service-accounts delete "$sa"
fi

echo "left in place: Artifact Registry repo logs-bench; check for orphaned disks with"
echo "  gcloud --project $project compute disks list --filter='name~^pvc-'"

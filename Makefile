# A benchmark is assembled from small make targets:
#
#   install-<engine> / uninstall-<engine> POOL=<instance> BENCH=<dir> - one instance
#   install-all-systems / uninstall-all-systems BENCH=<dir> - one instance per engine, POOL = engine name
#   deploy-bench / undeploy-bench BENCH=<dir> RUN=<name> - the run
#   monitoring-up / monitoring-down - VictoriaMetrics + Grafana
#   grafana / status - read-only helpers
#   *-dev - the same on the local kind cluster (deploy-dev builds the image)
#
# Cluster-touching targets take CONTEXT=<kubectl context>; *-dev targets supply
# the kind context themselves.
#
# BENCH is a benchmark directory under deploy/benchmarks/: it holds the sizing
# overlay for every engine it uses (systems/<engine>.yaml) and its run configs
# (RUN=<name> selects $(BENCH)/<name>.yaml).
# An engine install layers its helm values in this order: the engine file
# (deploy/systems/<engine>.yml), the benchmark's sizing overlay, EXTRA_VALUES,
# then the pin on stdin.
#
# POOL names the instance and is required. It is one identity: node pool
# label/taint, helm release, pod label, run-config `name`, `system` metric
# label - all equal.
# Placement comes from the engine's pin file (deploy/systems/pin/), or the raw
# manifest itself, with ${POOL} substituted at install time.
# One namespace (NS) holds the systems and the bench, one run at a time;
# monitoring lives in MON_NS, so wiping NS between runs keeps the results.
#
# See README.md and deploy/benchmarks/*/README.md.

KIND_CLUSTER := logs-bench-dev
IMAGE_REPO ?= docker.io/victoriametrics/logs-benchmark
# dev tag = source checksum: code change rolls the pods, no change = no-op
TAG := $(shell find cmd internal go.mod go.sum Dockerfile -type f -print0 | sort -z | xargs -0 shasum | shasum | cut -c1-12)
IMAGE := logs-bench:$(TAG)

# Deleting NS between runs also removes the PVCs, so the next run starts from
# empty systems.
NS     ?= logs-bench
MON_NS ?= logs-bench-monitoring

# Chart versions are PINNED so a measured number ties to the exact software that
# produced it and a re-run months later is the same experiment (image tags are
# pinned in deploy/systems/* where the chart exposes them; the chart version
# pins the rest: default images, sidecars, resource shapes, service names).
# Bump deliberately.
VICTORIALOGS_CHART_VERSION ?= 0.13.10
VMSTACK_CHART_VERSION      ?= 0.91.2
ECK_CHART_VERSION          ?= 3.5.0
SIGNOZ_CHART_VERSION       ?= 0.145.0
LOKI_CHART_VERSION         ?= 18.15.1
OPENSEARCH_CHART_VERSION   ?= 3.9.0
CLICKSTACK_CHART_VERSION   ?= 3.4.0
CLICKSTACK_OPS_CHART_VERSION ?= 1.1.0

# Fills the instance name into a pin file / raw manifest on stdout, to be piped
# into `helm -f -` or `kubectl apply -f -`.
PIN = sed 's/$${POOL}/$(POOL)/g'

# The benchmark directory. It supplies every engine's sizing (PVC size, storage
# class, JVM heap), which the engine files leave out, and its own run configs.
BENCH ?=

# The run config: BENCH + RUN names one inside the benchmark, CONFIG takes an
# explicit path, which may point outside the repo.
RUN    ?=
CONFIG ?= $(if $(and $(BENCH),$(RUN)),$(BENCH)/$(RUN).yaml)

# Optional per-instance values layer for a helm engine install (e.g. another
# image tag), applied between the sizing overlay and the pin.
EXTRA_VALUES ?=
XVALS = $(if $(EXTRA_VALUES),-f $(EXTRA_VALUES))

HELM    = helm --kube-context $(CONTEXT) --namespace $(NS)
KUBECTL = kubectl --context $(CONTEXT) --namespace $(NS)

ENGINES := victorialogs elasticsearch opensearch loki signoz clickstack

GOLANGCI_LINT_VERSION := 2.14.0

.PHONY: build test fmt vet check-all golangci-lint install-golangci-lint govulncheck \
	kind-up kind-down image kind-load release \
	check-context check-pool check-bench check-config check-no-pool helm-repos \
	$(addprefix install-,$(ENGINES)) $(addprefix uninstall-,$(ENGINES)) \
	install-all-systems uninstall-all-systems \
	deploy-bench undeploy-bench deploy-dev undeploy-dev \
	monitoring-up monitoring-down monitoring-up-dev monitoring-down-dev \
	grafana status

build:
	go build -o bin/bench ./cmd/bench

test:
	go test ./...

fmt:
	gofmt -l -w -s ./cmd ./internal

vet:
	go vet ./...

check-all: fmt vet golangci-lint govulncheck

golangci-lint: install-golangci-lint
	golangci-lint run

install-golangci-lint:
	which golangci-lint && (golangci-lint --version | grep -q $(GOLANGCI_LINT_VERSION)) || curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v$(GOLANGCI_LINT_VERSION)

govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --config deploy/kind/kind.yml

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

image:
	docker build -t $(IMAGE) .

kind-load: image
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

# public release image; needs docker login. The checks tie each image to one
# commit.
release:
	@case "$(TAG)" in v*) ;; *) echo "usage: make release TAG=vX.Y.Z"; exit 1;; esac
	@test -z "$$(git status --porcelain)" || { echo "commit or stash your changes first"; exit 1; }
	@test "$$(git rev-parse -q --verify 'refs/tags/$(TAG)^{commit}')" = "$$(git rev-parse HEAD)" || { echo "tag $(TAG) must point at HEAD"; exit 1; }
	docker buildx build --platform linux/amd64,linux/arm64 -t $(IMAGE_REPO):$(TAG) --push .

# Cluster-touching targets refuse to guess the cluster, and confirm before
# running against any context but the kind dev cluster (CONFIRMED=1 skips the
# prompt for scripted callers).
check-context:
	@test -n "$(CONTEXT)" || { echo "set CONTEXT to a kubectl context, e.g. make $(word 1,$(MAKECMDGOALS)) CONTEXT=<ctx>"; exit 1; }
	@{ test "$(CONTEXT)" = "kind-$(KIND_CLUSTER)" || test -n "$(CONFIRMED)"; } || { \
		printf 'run "%s" on context "%s"? (y/N) ' "$(MAKECMDGOALS)" "$(CONTEXT)"; \
		read reply; \
		case "$$reply" in [yY]) ;; *) echo aborted; exit 1;; esac; \
	}

# POOL must be a valid k8s label value / helm release name; it is also spliced
# into sed and shell, so the strict charset doubles as an injection guard.
check-pool:
	@test -n "$(POOL)" || { echo "POOL is required: set POOL=<instance name> (the node pool label, the helm release, and the run-config name); there is no default"; exit 1; }
	@echo "$(POOL)" | grep -Eq '^[a-z0-9]([a-z0-9-]{0,51}[a-z0-9])?$$' || \
		{ echo "POOL must be a lowercase RFC-1123 label (got '$(POOL)')"; exit 1; }

check-bench:
	@test -n "$(BENCH)" || { echo "set BENCH to a benchmark directory, e.g. make $(word 1,$(MAKECMDGOALS)) BENCH=deploy/benchmarks/all-systems"; exit 1; }
	@test -d "$(BENCH)" || { echo "BENCH directory not found: $(BENCH)"; exit 1; }

check-config:
	@test -n "$(CONFIG)" || { echo "set BENCH and RUN, e.g. BENCH=deploy/benchmarks/all-systems RUN=ingest, or CONFIG=<path to a run config>"; exit 1; }
	@test -f "$(CONFIG)" || { echo "run config not found: $(CONFIG)"; exit 1; }

helm-repos:
	helm repo add vm https://victoriametrics.github.io/helm-charts/ 2>/dev/null || true
	helm repo add elastic https://helm.elastic.co 2>/dev/null || true
	helm repo add signoz https://charts.signoz.io 2>/dev/null || true
	helm repo add grafana https://grafana.github.io/helm-charts 2>/dev/null || true
	helm repo add grafana-community https://grafana-community.github.io/helm-charts 2>/dev/null || true
	helm repo add opensearch https://opensearch-project.github.io/helm-charts/ 2>/dev/null || true
	helm repo add clickstack https://clickhouse.github.io/ClickStack-helm-charts 2>/dev/null || true

## Systems under test - one install/uninstall pair per engine, each installing
## ONE instance: the engine values file, the benchmark's sizing overlay,
## EXTRA_VALUES, then the pin file with ${POOL} filled in.
## Engine-specific extras (operators, one-off jobs, sibling services) and the
## teardown ordering they need live inside their own engine's target.

# The engine files carry no sizing, so an install without the benchmark's
# overlay would create an instance with no volume or no heap.
CHECK_SIZING = @test -f "$(BENCH)/systems/$(1).yaml" || { echo "no sizing overlay for $(1) in $(BENCH): expected $(BENCH)/systems/$(1).yaml"; exit 1; }

install-victorialogs: check-context check-pool check-bench helm-repos
	$(call CHECK_SIZING,victorialogs)
	$(PIN) deploy/systems/pin/victorialogs.yaml | $(HELM) upgrade --install $(POOL) vm/victoria-logs-single --version $(VICTORIALOGS_CHART_VERSION) \
		--create-namespace -f deploy/systems/victorialogs.yml -f $(BENCH)/systems/victorialogs.yaml $(XVALS) -f -

uninstall-victorialogs: check-context check-pool
	$(HELM) uninstall $(POOL) || true

# ES = the ECK operator (shared per namespace, fixed release name) + one
# Elasticsearch CR per instance, rendered by the local chart in deploy/charts.
install-elasticsearch: check-context check-pool check-bench helm-repos
	$(call CHECK_SIZING,elasticsearch)
	$(HELM) upgrade --install --wait eck elastic/eck-operator --version $(ECK_CHART_VERSION) --create-namespace
	$(PIN) deploy/systems/pin/elasticsearch.yaml | $(HELM) upgrade --install $(POOL) deploy/charts/elasticsearch \
		--create-namespace -f deploy/systems/elasticsearch.yml -f $(BENCH)/systems/elasticsearch.yaml $(XVALS) -f -

# `--wait` so the Elasticsearch CR is gone before the check below decides
# whether the operator is still needed.
uninstall-elasticsearch: check-context check-pool
	$(HELM) uninstall $(POOL) --wait || true
	@if ! $(KUBECTL) get elasticsearch -o name 2>/dev/null | grep -q .; then \
		$(HELM) uninstall eck 2>/dev/null || true; \
	fi

install-opensearch: check-context check-pool check-bench helm-repos
	$(call CHECK_SIZING,opensearch)
	$(PIN) deploy/systems/pin/opensearch.yaml | $(HELM) upgrade --install $(POOL) opensearch/opensearch --version $(OPENSEARCH_CHART_VERSION) \
		--create-namespace -f deploy/systems/opensearch.yml -f $(BENCH)/systems/opensearch.yaml $(XVALS) -f -

uninstall-opensearch: check-context check-pool
	$(HELM) uninstall $(POOL) || true

install-loki: check-context check-pool check-bench helm-repos
	$(call CHECK_SIZING,loki)
	$(PIN) deploy/systems/pin/loki.yaml | $(HELM) upgrade --install $(POOL) grafana-community/loki --version $(LOKI_CHART_VERSION) \
		--create-namespace -f deploy/systems/loki.yml -f $(BENCH)/systems/loki.yaml $(XVALS) -f -

uninstall-loki: check-context check-pool
	$(HELM) uninstall $(POOL) || true

# SigNoz = the chart + a one-off register Job (creates the first org so the
# collector gets its pipeline config; see signoz-register.yml). delete-then-
# apply reruns the Job on upgrade: a Job's pod template is immutable, so a
# plain re-apply would be rejected.
install-signoz: check-context check-pool check-bench helm-repos
	$(call CHECK_SIZING,signoz)
	$(PIN) deploy/systems/pin/signoz.yaml | $(HELM) upgrade --install $(POOL) signoz/signoz --version $(SIGNOZ_CHART_VERSION) \
		--create-namespace -f deploy/systems/signoz.yml -f $(BENCH)/systems/signoz.yaml $(XVALS) -f -
	$(KUBECTL) delete job $(POOL)-register --ignore-not-found
	$(PIN) deploy/systems/signoz-register.yml | $(KUBECTL) apply -f -

# Drain ClickHouse via its operator BEFORE uninstalling the release, which
# removes that operator: otherwise its ClickHouse StatefulSet is orphaned,
# keeps pod + PVC alive, and hangs the namespace teardown. The finalizer patch
# is a fallback when the operator is already gone. `--all` assumes one SigNoz
# instance per namespace (its CHI carries no per-release label).
uninstall-signoz: check-context check-pool
	$(KUBECTL) delete job $(POOL)-register --ignore-not-found
	-$(KUBECTL) delete clickhouseinstallation --all --ignore-not-found --timeout=120s
	-$(KUBECTL) get clickhouseinstallation -o name 2>/dev/null | xargs -I{} \
		$(KUBECTL) patch {} --type=merge -p '{"metadata":{"finalizers":null}}'
	$(HELM) uninstall $(POOL) || true

# ClickStack = shared operators (fixed release name) + the stack + a sibling
# Service exposing the HyperDX REST API for the querier.
install-clickstack: check-context check-pool check-bench helm-repos
	$(call CHECK_SIZING,clickstack)
	$(HELM) upgrade --install --wait clickstack-operators clickstack/clickstack-operators --version $(CLICKSTACK_OPS_CHART_VERSION) --create-namespace
	$(PIN) deploy/systems/pin/clickstack.yaml | $(HELM) upgrade --install $(POOL) clickstack/clickstack --version $(CLICKSTACK_CHART_VERSION) \
		--create-namespace -f deploy/systems/clickstack.yml -f $(BENCH)/systems/clickstack.yaml $(XVALS) -f -
	$(PIN) deploy/systems/clickstack-hdx-api.yml | $(KUBECTL) apply -f -

# Drain the operator-owned CRs BEFORE uninstalling the release/operators: same
# orphan/hang risk as SigNoz's ClickHouse, plus the operator webhooks refuse the
# delete once their operator is gone. `--all` assumes one ClickStack instance
# per namespace.
uninstall-clickstack: check-context check-pool
	-$(KUBECTL) delete service $(POOL)-hdx-api --ignore-not-found
	-$(KUBECTL) delete clickhousecluster,keepercluster --all --ignore-not-found --timeout=120s
	-$(KUBECTL) delete mongodbcommunity --all --ignore-not-found --timeout=120s
	$(HELM) uninstall $(POOL) || true
	@if ! $(KUBECTL) get clickhousecluster -o name 2>/dev/null | grep -q .; then \
		$(HELM) uninstall clickstack-operators 2>/dev/null || true; \
	fi

# One instance per engine on a node pool named after the engine, so each
# per-engine target gets POOL=<engine>. An explicit POOL is refused: one
# command-line POOL cannot stand in for every engine.
check-no-pool:
	@test -z "$(POOL)" || { echo "the all-systems targets name each engine's pool after the engine; use a per-engine target for a custom POOL"; exit 1; }
install-all-systems: check-no-pool check-context check-bench
	@for e in $(ENGINES); do \
		$(MAKE) install-$$e POOL=$$e CONTEXT="$(CONTEXT)" BENCH="$(BENCH)" CONFIRMED=1 || exit 1; \
	done
uninstall-all-systems: check-no-pool check-context
	@for e in $(ENGINES); do \
		$(MAKE) uninstall-$$e POOL=$$e CONTEXT="$(CONTEXT)" BENCH="$(BENCH)" CONFIRMED=1 || exit 1; \
	done

## The benchmark itself (writer + querier + dashboards): one run config
## (BENCH=<benchmark dir> RUN=<name>) deployed next to the systems.

deploy-bench: check-config check-context
	$(HELM) upgrade --install bench deploy/chart --create-namespace -f $(CONFIG) $(HELM_SET)

undeploy-bench: check-context
	$(HELM) uninstall bench || true

## Monitoring (VictoriaMetrics + Grafana + scrape config) in $(MON_NS). The
## bench VMPodScrape discovers bench pods across namespaces by label, so a
## fresh run is scraped without touching monitoring.

HELM_MON    = helm --kube-context $(CONTEXT) --namespace $(MON_NS)
KUBECTL_MON = kubectl --context $(CONTEXT) --namespace $(MON_NS)

monitoring-up: check-context helm-repos
	$(HELM_MON) upgrade --install --wait vms vm/victoria-metrics-k8s-stack \
		--version $(VMSTACK_CHART_VERSION) \
		--create-namespace --values deploy/monitoring/values.yml
	$(KUBECTL_MON) apply -f deploy/monitoring/podscrape.yaml

monitoring-down: check-context
	# `-`: tolerate a missing VMPodScrape CRD (monitoring never installed here);
	# --ignore-not-found only covers a missing OBJECT, not a missing KIND
	-$(KUBECTL_MON) delete -f deploy/monitoring/podscrape.yaml --ignore-not-found
	$(HELM_MON) uninstall vms || true

## Dev loop on kind: one VictoriaLogs pinned to the kind node (labeled in
## deploy/kind/kind.yml) + the bench with a locally built image. Sub-make calls
## so CONFIG/NS/CONTEXT derive exactly like a production call.

deploy-dev: kind-load
	$(MAKE) install-victorialogs POOL=victorialogs CONTEXT=kind-$(KIND_CLUSTER) \
		BENCH=deploy/benchmarks/dev
	$(MAKE) deploy-bench CONTEXT=kind-$(KIND_CLUSTER) BENCH=deploy/benchmarks/dev RUN=query \
		HELM_SET="--set image.repository=logs-bench --set image.tag=$(TAG) --set image.pullPolicy=Never"

undeploy-dev:
	$(MAKE) undeploy-bench CONTEXT=kind-$(KIND_CLUSTER)
	$(MAKE) uninstall-victorialogs POOL=victorialogs CONTEXT=kind-$(KIND_CLUSTER)

monitoring-up-dev:
	$(MAKE) monitoring-up CONTEXT=kind-$(KIND_CLUSTER)

monitoring-down-dev:
	$(MAKE) monitoring-down CONTEXT=kind-$(KIND_CLUSTER)

## Read-only helpers; they default to the dev cluster, pass CONTEXT for another.

grafana status: CONTEXT ?= kind-$(KIND_CLUSTER)

grafana:
	@echo "Grafana: http://localhost:3000 (admin / $$($(KUBECTL_MON) get secret vms-grafana -o jsonpath='{.data.admin-password}' | base64 -d))"
	$(KUBECTL_MON) port-forward svc/vms-grafana 3000:80

status:
	$(KUBECTL) get pods,pvc
	-@$(KUBECTL_MON) get pods 2>/dev/null

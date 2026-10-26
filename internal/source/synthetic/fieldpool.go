package synthetic

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/brianvoe/gofakeit/v7"
)

// fieldKind says how a pool field's value behaves within one app.
type fieldKind int

const (
	fieldConst   fieldKind = iota // one value picked per app, fixed for the run
	fieldEnum                     // drawn per line from a small weighted set
	fieldDynamic                  // generated per line
)

// PoolField is one candidate field an app schema can include. Only Name and
// Numeric are needed by encoders; how the value is produced stays internal.
type PoolField struct {
	Name    string
	Numeric bool // the value is an integer, encoded as a number
	kind    fieldKind
	values  []string                       // const: candidates; enum: the value set
	weights []int                          // enum only, pairs with values
	gen     func(f *gofakeit.Faker) string // dynamic only
}

// draw produces one per-line value for an enum or dynamic field.
func (p *PoolField) draw(f *gofakeit.Faker) string {
	if p.kind == fieldDynamic {
		return p.gen(f)
	}
	return pickWeighted(f, p.values, p.weights)
}

// appSchema draws one app's slice of the field pool, in pool order.
func appSchema(f *gofakeit.Faker) []PoolField {
	n := f.Number(minAppFields, maxAppFields)
	idx := make([]int, len(fieldPool))
	for i := range idx {
		idx[i] = i
	}
	f.ShuffleInts(idx)
	sel := idx[:n]
	sort.Ints(sel)
	schema := make([]PoolField, n)
	for i, j := range sel {
		schema[i] = fieldPool[j]
	}
	return schema
}

// pickWeighted returns one of values, distributed by the matching weights.
func pickWeighted(f *gofakeit.Faker, values []string, weights []int) string {
	total := 0
	for _, w := range weights {
		total += w
	}
	r := f.Number(1, total)
	for i, w := range weights {
		r -= w
		if r <= 0 {
			return values[i]
		}
	}
	return values[len(values)-1]
}

// Each app draws this many pool fields, so a line carries 30-40 fields total:
// the 5 core fields (timestamp, host, app, level, message), the 3 host fields,
// and the app's slice.
const (
	minAppFields = 22
	maxAppFields = 32
)

// Log level values and weights for the core `level` field: mostly info, few
// errors, mirroring a healthy production service.
var (
	logLevels       = []string{"debug", "info", "warning", "error"}
	logLevelWeights = []int{10, 80, 7, 3}
)

// hostFields are the host-level fields: the same keys on every host, one
// value picked per host.
var hostFields = []PoolField{
	{Name: "zone", kind: fieldConst, values: []string{"us-east1-b", "us-east1-c", "us-east1-d", "us-central1-a", "us-central1-b", "europe-west1-b", "europe-west1-c", "asia-southeast1-a"}},
	{Name: "instance_type", kind: fieldConst, values: []string{"n2d-standard-4", "n2d-standard-8", "n2d-standard-16", "n2d-highmem-8", "c2d-highcpu-16", "e2-standard-8"}},
	{Name: "os_image", kind: fieldConst, values: []string{"cos-109-lts", "cos-113-lts", "ubuntu-2204-lts", "ubuntu-2404-lts"}},
}

// fieldPool is what app schemas draw from. The class mix is the workload's
// value distribution: mostly constant fields, a few low-cardinality enums and
// high-cardinality dynamic fields varying per line.
var fieldPool = []PoolField{
	// constant: one value per app, fixed for the run
	{Name: "namespace", kind: fieldConst, values: []string{"payments", "checkout", "auth", "search", "catalog", "media", "messaging", "analytics", "platform", "edge"}},
	{Name: "team", kind: fieldConst, values: []string{"core", "growth", "sre", "data", "identity", "infra", "search", "media", "platform", "payments"}},
	{Name: "env", kind: fieldConst, values: []string{"prod", "staging", "canary", "dev"}},
	{Name: "tier", kind: fieldConst, values: []string{"frontend", "backend", "batch", "cache", "gateway"}},
	{Name: "component", kind: fieldConst, values: []string{"api", "worker", "scheduler", "ui", "proxy", "consumer"}},
	{Name: "part_of", kind: fieldConst, values: []string{"storefront", "billing", "identity", "logistics", "search-suite", "media-stack"}},
	{Name: "managed_by", kind: fieldConst, values: []string{"helm", "argocd", "kustomize", "flux"}},
	{Name: "release_channel", kind: fieldConst, values: []string{"stable", "beta", "edge"}},
	{Name: "app_version", kind: fieldConst, values: []string{"1.0.3", "1.4.0", "2.1.7", "2.3.1", "3.0.2", "0.9.8", "4.2.0", "1.12.5"}},
	{Name: "image_digest", kind: fieldConst, values: []string{"a3f9c2e1", "91d04b7f", "5c22ae90", "fe0187d3", "3b6c5a12", "c47e92b8"}},
	{Name: "build_id", kind: fieldConst, values: []string{"2481", "2513", "2600", "2744", "2812", "2901"}},
	{Name: "vcs_branch", kind: fieldConst, values: []string{"main", "release-1.4", "release-2.1", "hotfix-auth"}},
	{Name: "runtime", kind: fieldConst, values: []string{"go1.24", "openjdk-21", "python-3.12", "node-22", "dotnet-8", "rust-1.79"}},
	{Name: "sdk_version", kind: fieldConst, values: []string{"otel-1.27.0", "otel-1.28.1", "otel-1.30.0", "otel-1.31.2"}},
	{Name: "service_mesh", kind: fieldConst, values: []string{"istio", "linkerd", "none"}},
	{Name: "ingress_class", kind: fieldConst, values: []string{"nginx", "traefik", "envoy", "none"}},
	{Name: "autoscaler", kind: fieldConst, values: []string{"hpa", "keda", "none"}},
	{Name: "replicas", kind: fieldConst, Numeric: true, values: []string{"1", "2", "3", "4", "6", "8"}},
	{Name: "cpu_request", kind: fieldConst, values: []string{"250m", "500m", "1", "2"}},
	{Name: "memory_request", kind: fieldConst, values: []string{"256Mi", "512Mi", "1Gi", "2Gi", "4Gi"}},
	{Name: "storage_class", kind: fieldConst, values: []string{"standard", "premium-ssd", "none"}},
	{Name: "priority_class", kind: fieldConst, values: []string{"system-high", "default", "batch-low"}},
	{Name: "qos_class", kind: fieldConst, values: []string{"Guaranteed", "Burstable", "BestEffort"}},
	{Name: "security_profile", kind: fieldConst, values: []string{"restricted", "baseline"}},
	{Name: "service_account", kind: fieldConst, values: []string{"default", "app-runner", "batch-runner", "ingest-sa"}},
	{Name: "cost_center", kind: fieldConst, values: []string{"cc-1001", "cc-1015", "cc-2203", "cc-3410", "cc-5120"}},
	{Name: "criticality", kind: fieldConst, values: []string{"p1", "p2", "p3", "p4"}},
	{Name: "sla_tier", kind: fieldConst, values: []string{"gold", "silver", "bronze"}},
	{Name: "on_call", kind: fieldConst, values: []string{"platform-primary", "payments-primary", "search-primary", "data-primary", "sre-primary"}},
	{Name: "kafka_topic", kind: fieldConst, values: []string{"events-orders", "events-users", "events-clicks", "events-payments", "events-audit"}},
	{Name: "db_pool", kind: fieldConst, values: []string{"pg-main", "pg-replica", "mysql-main", "none"}},
	{Name: "cache_backend", kind: fieldConst, values: []string{"redis", "memcached", "none"}},
	{Name: "queue_backend", kind: fieldConst, values: []string{"kafka", "rabbitmq", "sqs", "none"}},
	{Name: "feature_set", kind: fieldConst, values: []string{"alpha", "standard", "extended"}},
	{Name: "tenant_mode", kind: fieldConst, values: []string{"shared", "dedicated"}},
	{Name: "backup_policy", kind: fieldConst, values: []string{"hourly", "daily", "none"}},

	// enum: low cardinality, drawn per line from a weighted set
	{Name: "method", kind: fieldEnum, values: []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD"}, weights: []int{70, 15, 5, 4, 3, 3}},
	{Name: "protocol", kind: fieldEnum, values: []string{"HTTP/1.1", "HTTP/2.0", "HTTP/1.0"}, weights: []int{60, 35, 5}},
	{Name: "status", kind: fieldEnum, Numeric: true, values: []string{"200", "301", "302", "400", "404", "500"}, weights: []int{85, 3, 3, 4, 3, 2}},
	{Name: "cache_status", kind: fieldEnum, values: []string{"hit", "miss", "bypass"}, weights: []int{70, 25, 5}},
	{Name: "retries", kind: fieldEnum, Numeric: true, values: []string{"0", "1", "2"}, weights: []int{90, 8, 2}},
	{Name: "tls_version", kind: fieldEnum, values: []string{"TLSv1.3", "TLSv1.2"}, weights: []int{85, 15}},
	{Name: "compression", kind: fieldEnum, values: []string{"gzip", "br", "none"}, weights: []int{60, 25, 15}},
	{Name: "sampled", kind: fieldEnum, values: []string{"false", "true"}, weights: []int{90, 10}},
	{Name: "content_type", kind: fieldEnum, values: []string{"application/json", "text/html", "application/x-protobuf"}, weights: []int{70, 20, 10}},

	// dynamic: high cardinality, generated per line
	{Name: "path", kind: fieldDynamic, gen: randResourceURI},
	{Name: "ip", kind: fieldDynamic, gen: func(f *gofakeit.Faker) string { return f.IPv4Address() }},
	{Name: "user_agent", kind: fieldDynamic, gen: func(f *gofakeit.Faker) string { return f.UserAgent() }},
	{Name: "user_id", kind: fieldDynamic, gen: randAuthUserID},
	{Name: "referer", kind: fieldDynamic, gen: func(f *gofakeit.Faker) string { return f.URL() }},
	{Name: "bytes_sent", kind: fieldDynamic, Numeric: true, gen: func(f *gofakeit.Faker) string { return strconv.Itoa(f.Number(0, 30000)) }},
	{Name: "request_id", kind: fieldDynamic, gen: func(f *gofakeit.Faker) string { return f.UUID() }},
	{Name: "span_id", kind: fieldDynamic, gen: func(f *gofakeit.Faker) string { return fmt.Sprintf("%016x", f.Uint64()) }},
	{Name: "duration_ms", kind: fieldDynamic, Numeric: true, gen: func(f *gofakeit.Faker) string { return strconv.Itoa(f.Number(0, 5000)) }},
}

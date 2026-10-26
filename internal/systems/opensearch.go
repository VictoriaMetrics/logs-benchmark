package systems

import (
	"context"
	"fmt"
)

type opensearch struct {
	addr string // e.g. http://opensearch-master:9200
}

const osTarget = "logs-bench"

const osTemplateName = "logs-bench"

// osIndexTemplate pins the core fields' types, mirroring Elasticsearch's
// built-in logs template (OpenSearch ships none); every other field falls to
// the dynamic default (text + a .keyword multi-field). message uses
// match_only_text - the cheap text variant that still answers match_phrase.
const osIndexTemplate = `{
  "index_patterns": ["%s*"],
  "template": {
    "settings": {"number_of_replicas": 0},
    "mappings": {
      "properties": {
        "@timestamp": {"type": "date"},
        "level":      {"type": "keyword"},
        "app":        {"type": "keyword"},
        "message":    {"type": "match_only_text"},
        "host":       {"properties": {"name": {"type": "keyword"}}}
      }
    }
  }
}`

func (s *opensearch) WaitReady() error {
	url := s.addr + "/_cluster/health?wait_for_status=yellow&timeout=5s"
	if err := waitHTTP(url, esClusterReady); err != nil {
		return err
	}
	return s.ensureTemplate()
}

// ensureTemplate installs osIndexTemplate before any log is indexed.
func (s *opensearch) ensureTemplate() error {
	body := fmt.Appendf(nil, osIndexTemplate, osTarget)
	url := s.addr + "/_index_template/" + osTemplateName
	if _, _, _, err := doReq(context.Background(), "PUT", url, "application/json", body); err != nil {
		return fmt.Errorf("set index template: %w", err)
	}
	return nil
}

func (s *opensearch) PrepareBatch(entries []LogEntry) (Batch, error) {
	return esBulkBatch(entries), nil
}

func (s *opensearch) Send(ctx context.Context, b Batch) (int, error) {
	return esBulkSend(ctx, s.addr+"/"+osTarget+"/_bulk", b)
}

func (s *opensearch) Queries(p QueryParams) []Query {
	return esQuerySet(s.addr, osTarget, p.App)
}

func (s *opensearch) StoredRows(ctx context.Context) (*int64, error) {
	return esStoredRows(ctx, s.addr, osTarget)
}

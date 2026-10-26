package systems

import (
	"fmt"
	"strconv"

	"github.com/VictoriaMetrics/easyproto"
)

// otlpContentType is the OTLP/HTTP content type of a protobuf-encoded request.
const otlpContentType = "application/x-protobuf"

var otlpMarshalerPool easyproto.MarshalerPool

// otlpBatch encodes the records as a protobuf ExportLogsServiceRequest, with one
// ResourceLogs per (host, app) carrying host.name and service.name. Each log
// record has the time, the level as severity text and number, the message as
// the body, and every other field as an attribute.
//
// Field numbers are from opentelemetry-proto:
// https://github.com/open-telemetry/opentelemetry-proto/blob/a5f0eac5b802f7ae51dfe41e5116fe5548955e64/opentelemetry/proto/logs/v1/logs.proto
// https://github.com/open-telemetry/opentelemetry-proto/blob/a5f0eac5b802f7ae51dfe41e5116fe5548955e64/opentelemetry/proto/common/v1/common.proto
func otlpBatch(entries []LogEntry) (Batch, error) {
	m := otlpMarshalerPool.Get()
	defer otlpMarshalerPool.Put(m)

	req := m.MessageMarshaler()
	for _, stream := range groupByStream(entries) {
		rl := req.AppendMessage(1) // ExportLogsServiceRequest.resource_logs

		res := rl.AppendMessage(1) // ResourceLogs.resource
		otlpAppendStringAttr(res, 1, "host.name", stream[0].Rec.Host.Name)
		otlpAppendStringAttr(res, 1, "service.name", stream[0].Rec.App.Name)

		sl := rl.AppendMessage(2) // ResourceLogs.scope_logs
		for _, e := range stream {
			rec := e.Rec
			lr := sl.AppendMessage(2) // ScopeLogs.log_records
			lr.AppendFixed64(1, uint64(e.Ts.UnixNano()))
			lr.AppendInt32(2, otlpSeverityNumber(rec.Level))
			lr.AppendString(3, rec.Level)
			lr.AppendMessage(5).AppendString(1, rec.Message) // body: AnyValue.string_value
			for _, fd := range rec.Host.Consts {
				if err := otlpAppendAttr(lr, 6, fd.Name, fd.Value, fd.Numeric); err != nil {
					return Batch{}, err
				}
			}
			for _, fd := range rec.App.Consts {
				if err := otlpAppendAttr(lr, 6, fd.Name, fd.Value, fd.Numeric); err != nil {
					return Batch{}, err
				}
			}
			for i, lf := range rec.App.Lines {
				if err := otlpAppendAttr(lr, 6, lf.Name, rec.LineVals[i], lf.Numeric); err != nil {
					return Batch{}, err
				}
			}
		}
	}
	body := m.Marshal(make([]byte, 0, len(entries)*1024))
	return Batch{Body: body, Lines: len(entries)}, nil
}

// otlpAppendAttr appends a KeyValue under fieldNum, with an integer field as
// AnyValue.int_value and any other as AnyValue.string_value.
func otlpAppendAttr(mm *easyproto.MessageMarshaler, fieldNum uint32, key, value string, numeric bool) error {
	if !numeric {
		otlpAppendStringAttr(mm, fieldNum, key, value)
		return nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("field %q: numeric value %q is not an integer", key, value)
	}
	kv := mm.AppendMessage(fieldNum)
	kv.AppendString(1, key)
	kv.AppendMessage(2).AppendInt64(3, n)
	return nil
}

func otlpAppendStringAttr(mm *easyproto.MessageMarshaler, fieldNum uint32, key, value string) {
	kv := mm.AppendMessage(fieldNum)
	kv.AppendString(1, key)
	kv.AppendMessage(2).AppendString(1, value)
}

// otlpSeverityNumber maps the generator's levels to the SeverityNumber enum.
func otlpSeverityNumber(level string) int32 {
	switch level {
	case "debug":
		return 5 // SEVERITY_NUMBER_DEBUG
	case "info":
		return 9 // SEVERITY_NUMBER_INFO
	case "warning":
		return 13 // SEVERITY_NUMBER_WARN
	case "error":
		return 17 // SEVERITY_NUMBER_ERROR
	}
	return 0 // SEVERITY_NUMBER_UNSPECIFIED
}

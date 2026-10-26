package synthetic

import (
	"fmt"
	"time"
)

// EncodeFunc appends a record encoded in one format to dst and returns the
// extended slice (pass nil for a fresh buffer).
type EncodeFunc func(dst []byte, rec Record, ts time.Time) []byte

// Encoding is a named encoder: Name is a human-readable label for the format.
type Encoding struct {
	Name   string
	Encode EncodeFunc
}

// Built-in encodings. json and logfmt carry the full record except the syslog
// priority; rfc5424 is a fixed template carrying a subset.
var (
	JSON    = Encoding{Name: "json", Encode: AppendJSON}
	Logfmt  = Encoding{Name: "logfmt", Encode: AppendLogfmt}
	RFC5424 = Encoding{Name: "rfc5424", Encode: AppendRFC5424}
)

// AppendJSON appends rec as a JSON object to dst. Field order is fixed: the
// core fields, then the host and app constants, then the app's per-line fields;
// JSON key order is not significant, so consumers parse by name.
func AppendJSON(dst []byte, rec Record, ts time.Time) []byte {
	dst = append(dst, `{"timestamp":"`...)
	dst = ts.AppendFormat(dst, time.RFC3339Nano)
	dst = append(dst, '"')
	dst = appendJSONField(dst, "host", rec.Host.Name)
	dst = appendJSONField(dst, "app", rec.App.Name)
	dst = appendJSONField(dst, "level", rec.Level)
	dst = appendJSONField(dst, "message", rec.Message)
	dst = append(dst, rec.Host.ConstJSON...)
	dst = append(dst, rec.App.ConstJSON...)
	dst = AppendLineFields(dst, rec)
	return append(dst, '}')
}

// AppendLineFields appends rec's per-line app fields to dst as `,"key":value`
// JSON pairs, for use inside a flat JSON object.
func AppendLineFields(dst []byte, rec Record) []byte {
	for i, lf := range rec.App.Lines {
		dst = append(dst, ',')
		dst = AppendJSONString(dst, lf.Name)
		dst = append(dst, ':')
		if lf.Numeric {
			dst = append(dst, rec.LineVals[i]...)
		} else {
			dst = AppendJSONString(dst, rec.LineVals[i])
		}
	}
	return dst
}

// constJSON pre-encodes fields as `,"key":value` pairs for splicing into flat
// JSON encodings.
func constJSON(fields []Field) []byte {
	var b []byte
	for _, fd := range fields {
		b = append(b, ',')
		b = AppendJSONString(b, fd.Name)
		b = append(b, ':')
		if fd.Numeric {
			b = append(b, fd.Value...)
		} else {
			b = AppendJSONString(b, fd.Value)
		}
	}
	return b
}

// appendJSONField appends `,"key":"value"` with both key and value escaped.
func appendJSONField(dst []byte, key, val string) []byte {
	dst = append(dst, ',')
	dst = AppendJSONString(dst, key)
	dst = append(dst, ':')
	return AppendJSONString(dst, val)
}

// AppendJSONString appends s to dst as a quoted, escaped JSON string. It escapes
// the two mandatory characters (" and \) and control characters < 0x20; valid
// UTF-8 above that is copied verbatim (as encoding/json does without HTML
// escaping).
func AppendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			const hex = "0123456789abcdef"
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// AppendLogfmt appends rec as a logfmt line to dst.
func AppendLogfmt(dst []byte, rec Record, ts time.Time) []byte {
	dst = fmt.Appendf(dst, "ts=%s host=%s app=%s level=%s msg=%q",
		ts.Format(time.RFC3339Nano), rec.Host.Name, rec.App.Name, rec.Level, rec.Message)
	for _, fd := range rec.Host.Consts {
		dst = fmt.Appendf(dst, " %s=%s", fd.Name, fd.Value)
	}
	for _, fd := range rec.App.Consts {
		dst = fmt.Appendf(dst, " %s=%s", fd.Name, fd.Value)
	}
	for i, lf := range rec.App.Lines {
		dst = fmt.Appendf(dst, " %s=%s", lf.Name, rec.LineVals[i])
	}
	return dst
}

// AppendRFC5424 appends rec as an RFC 5424 syslog line to dst.
func AppendRFC5424(dst []byte, rec Record, ts time.Time) []byte {
	return fmt.Appendf(dst, "<%d>1 %s %s %s - - - %s",
		rec.Priority, ts.Format(time.RFC3339Nano), rec.Host.Name, rec.App.Name, rec.Message)
}

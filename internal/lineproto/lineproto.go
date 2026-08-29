// Package lineproto encodes and decodes InfluxDB line protocol.
//
// Line protocol was chosen as the on-disk format because it is append-only,
// self-describing, greppable with ordinary text tools, and directly ingestible
// by InfluxDB/Telegraf/VictoriaMetrics if the user ever wants to ship the data
// somewhere else. There is no index and no database process: a day of samples
// is just a text file, and yesterday's is the same text file with .zst on the
// end.
//
// One sample looks like:
//
//	uptime,endpoint=https://example.com/,kind=http,host=example.com up=1i,code=200i,rtt=42.1 1724900000000000000
package lineproto

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// DefaultMeasurement is the measurement name used unless overridden.
const DefaultMeasurement = "uptime"

// Precision controls timestamp resolution. Nanoseconds is the line protocol
// default; coarser settings make files smaller at the cost of ordering detail.
type Precision string

const (
	Nanoseconds  Precision = "ns"
	Microseconds Precision = "us"
	Milliseconds Precision = "ms"
	Seconds      Precision = "s"
)

// ParsePrecision validates a precision string.
func ParsePrecision(s string) (Precision, error) {
	switch Precision(s) {
	case Nanoseconds, Microseconds, Milliseconds, Seconds:
		return Precision(s), nil
	}
	return "", fmt.Errorf("precision must be one of ns, us, ms, s (got %q)", s)
}

func (p Precision) divisor() int64 {
	switch p {
	case Microseconds:
		return int64(time.Microsecond)
	case Milliseconds:
		return int64(time.Millisecond)
	case Seconds:
		return int64(time.Second)
	default:
		return 1
	}
}

// Encoder turns Results into line protocol bytes. It is safe for use by one
// goroutine; the store owns exactly one and serialises access to it.
type Encoder struct {
	Measurement string
	ExtraTags   []model.Header // static tags added to every point (e.g. host=laptop)
	Precision   Precision
}

// NewEncoder returns an Encoder with sane defaults filled in.
func NewEncoder(measurement string, precision Precision, extra []model.Header) *Encoder {
	if measurement == "" {
		measurement = DefaultMeasurement
	}
	if precision == "" {
		precision = Nanoseconds
	}
	return &Encoder{Measurement: measurement, ExtraTags: extra, Precision: precision}
}

// AppendResult appends one encoded line (including a trailing newline) to dst
// and returns the extended slice. Appending into a caller-owned buffer is what
// lets the batching writer accumulate ten minutes of samples with no
// per-sample allocation.
func (e *Encoder) AppendResult(dst []byte, r model.Result) []byte {
	dst = appendEscaped(dst, e.Measurement, escMeasurement)

	// --- tags. Sorted-by-convention: endpoint first, then the rest. Influx
	// wants tags sorted by key for best performance, and our fixed key set is
	// already written in sorted order (endpoint, group, host, kind).
	dst = append(dst, ",endpoint="...)
	dst = appendEscaped(dst, r.Target, escTag)
	if r.Group != "" {
		dst = append(dst, ",group="...)
		dst = appendEscaped(dst, r.Group, escTag)
	}
	if r.Host != "" {
		dst = append(dst, ",host="...)
		dst = appendEscaped(dst, r.Host, escTag)
	}
	if r.Kind != "" {
		dst = append(dst, ",kind="...)
		dst = appendEscaped(dst, string(r.Kind), escTag)
	}
	for _, t := range e.ExtraTags {
		dst = append(dst, ',')
		dst = appendEscaped(dst, t.Key, escTag)
		dst = append(dst, '=')
		dst = appendEscaped(dst, t.Value, escTag)
	}

	// --- fields
	dst = append(dst, " up="...)
	if r.Status == model.StatusUp {
		dst = append(dst, "1i"...)
	} else {
		dst = append(dst, "0i"...)
	}
	if r.Code != 0 {
		dst = append(dst, ",code="...)
		dst = strconv.AppendInt(dst, int64(r.Code), 10)
		dst = append(dst, 'i')
	}
	// RTT is always emitted, including for failures, because "how long did it
	// take to fail" is a real signal (timeout vs instant connection refused).
	dst = appendMillis(dst, ",rtt=", r.RTT)
	dst = appendMillisOmitZero(dst, ",dns=", r.DNS)
	dst = appendMillisOmitZero(dst, ",connect=", r.Connect)
	dst = appendMillisOmitZero(dst, ",tls=", r.TLS)
	dst = appendMillisOmitZero(dst, ",ttfb=", r.TTFB)
	if r.Bytes > 0 {
		dst = append(dst, ",bytes="...)
		dst = strconv.AppendInt(dst, r.Bytes, 10)
		dst = append(dst, 'i')
	}
	if r.Attempt > 1 {
		dst = append(dst, ",attempt="...)
		dst = strconv.AppendInt(dst, int64(r.Attempt), 10)
		dst = append(dst, 'i')
	}
	if r.Err != "" {
		dst = append(dst, ",err=\""...)
		dst = appendEscaped(dst, r.Err, escString)
		dst = append(dst, '"')
	}

	// --- timestamp
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, r.At.UnixNano()/e.Precision.divisor(), 10)
	return append(dst, '\n')
}

func appendMillis(dst []byte, key string, d time.Duration) []byte {
	dst = append(dst, key...)
	return strconv.AppendFloat(dst, float64(d)/float64(time.Millisecond), 'f', 3, 64)
}

func appendMillisOmitZero(dst []byte, key string, d time.Duration) []byte {
	if d <= 0 {
		return dst
	}
	return appendMillis(dst, key, d)
}

// escape classes, per the line protocol spec.
type escClass int

const (
	escMeasurement escClass = iota // , and space
	escTag                         // , = and space  (also field keys)
	escString                      // " and \
)

func appendEscaped(dst []byte, s string, class escClass) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch class {
		case escMeasurement:
			if c == ',' || c == ' ' {
				dst = append(dst, '\\')
			}
		case escTag:
			if c == ',' || c == ' ' || c == '=' {
				dst = append(dst, '\\')
			}
		case escString:
			if c == '"' || c == '\\' {
				dst = append(dst, '\\')
			}
			// Newlines would corrupt the append-only file; fold them.
			if c == '\n' || c == '\r' {
				dst = append(dst, ' ')
				continue
			}
		}
		dst = append(dst, c)
	}
	return dst
}

// ---------------------------------------------------------------------------
// Decoding
// ---------------------------------------------------------------------------

// ErrSkipLine is returned for blank lines and comments, which are not errors.
var ErrSkipLine = errors.New("lineproto: skip")

// ParseLine decodes one line back into a Result. It ignores measurement name
// and any tags or fields it does not know about, so hand-edited or
// externally-appended files still load.
func ParseLine(line string, prec Precision) (model.Result, error) {
	var r model.Result

	line = strings.TrimRight(line, "\r\n")
	if s := strings.TrimSpace(line); s == "" || strings.HasPrefix(s, "#") {
		return r, ErrSkipLine
	}

	tagsPart, rest, ok := cutUnescaped(line, ' ')
	if !ok {
		return r, fmt.Errorf("missing field set")
	}
	fieldsPart, tsPart, ok := cutFieldSet(rest)
	if !ok {
		// Timestamp is optional in line protocol; without one we cannot place
		// the sample in time, so treat it as unusable rather than guessing.
		return r, fmt.Errorf("missing timestamp")
	}

	// --- tags (first element is the measurement, which we discard)
	for i, kv := range splitUnescaped(tagsPart, ',') {
		if i == 0 {
			continue
		}
		k, v, ok := cutUnescaped(kv, '=')
		if !ok {
			continue
		}
		v = unescape(v)
		switch unescape(k) {
		case "endpoint":
			r.Target = v
		case "group":
			r.Group = v
		case "host":
			r.Host = v
		case "kind":
			r.Kind = model.Kind(v)
		}
	}
	if r.Target == "" {
		return r, fmt.Errorf("missing endpoint tag")
	}

	// --- fields
	for _, kv := range splitFields(fieldsPart) {
		k, v, ok := cutUnescaped(kv, '=')
		if !ok {
			continue
		}
		switch unescape(k) {
		case "up":
			if n, err := parseInt(v); err == nil && n == 1 {
				r.Status = model.StatusUp
			}
		case "code":
			if n, err := parseInt(v); err == nil {
				r.Code = int(n)
			}
		case "rtt":
			r.RTT = parseMillis(v)
		case "dns":
			r.DNS = parseMillis(v)
		case "connect":
			r.Connect = parseMillis(v)
		case "tls":
			r.TLS = parseMillis(v)
		case "ttfb":
			r.TTFB = parseMillis(v)
		case "bytes":
			if n, err := parseInt(v); err == nil {
				r.Bytes = n
			}
		case "attempt":
			if n, err := parseInt(v); err == nil {
				r.Attempt = int(n)
			}
		case "err":
			r.Err = unquote(v)
		}
	}
	if r.Attempt == 0 {
		r.Attempt = 1
	}

	// --- timestamp
	ts, err := strconv.ParseInt(strings.TrimSpace(tsPart), 10, 64)
	if err != nil {
		return r, fmt.Errorf("bad timestamp %q", tsPart)
	}
	r.At = time.Unix(0, ts*prec.divisor())
	return r, nil
}

func parseInt(v string) (int64, error) {
	return strconv.ParseInt(strings.TrimSuffix(v, "i"), 10, 64)
}

func parseMillis(v string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "i"), 64)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Millisecond))
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

// cutFieldSet splits "field set" from "timestamp". The separator is the first
// space that is neither escaped nor inside a quoted string value — string
// fields such as err="connection refused" legitimately contain spaces.
func cutFieldSet(s string) (fields, ts string, found bool) {
	inStr := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			inStr = !inStr
		case ' ':
			if !inStr {
				return s[:i], s[i+1:], true
			}
		}
	}
	return s, "", false
}

// cutUnescaped is strings.Cut but ignores separators preceded by a backslash.
func cutUnescaped(s string, sep byte) (before, after string, found bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func splitUnescaped(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// splitFields splits the field set on commas, but not on commas that sit
// inside a quoted string value (our `err="connection refused, retrying"`).
func splitFields(s string) []string {
	var out []string
	start, inStr := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			inStr = !inStr
		case ',':
			if !inStr {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func unescape(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

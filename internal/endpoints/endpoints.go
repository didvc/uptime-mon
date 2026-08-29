// Package endpoints parses endpoints.txt, the single source of truth for what
// gets probed.
//
// The format is one target per line, with optional key=value modifiers:
//
//	# comments and blank lines are ignored
//	https://example.com/
//	https://example.com/health   name="API health" interval=30s keyword=ok
//	ping://8.8.8.8               name=google-dns group=net
//	tcp://db.internal:5432       timeout=3s
//
// Values may be quoted with single or double quotes when they contain spaces.
// A "defaults" line sets the baseline for every target that follows it, which
// keeps large files readable without repeating the same interval 60 times.
package endpoints

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// Defaults are applied to every target that does not override them. The zero
// value is not useful; callers should start from BaseDefaults.
type Defaults struct {
	Interval      time.Duration
	Timeout       time.Duration
	Retries       int
	RetryInterval time.Duration
	MaxRedirects  int
	Insecure      bool
	UserAgent     string
	MaxBody       int64
	Group         string
	Accept        model.StatusRanges
}

// BaseDefaults is the built-in baseline: a one-minute interval matching the
// common Uptime Kuma default, a 10s timeout, and no retries.
func BaseDefaults() Defaults {
	return Defaults{
		Interval:      60 * time.Second,
		Timeout:       10 * time.Second,
		Retries:       0,
		RetryInterval: 20 * time.Second,
		MaxRedirects:  10,
		Insecure:      false,
		UserAgent:     "uptime-mon/1.0",
		MaxBody:       1 << 20, // 1 MiB is plenty for a keyword match
		Accept:        model.DefaultAccept(),
	}
}

// ParseError carries the line number so a typo in a 90-line file is findable.
type ParseError struct {
	Line int
	Text string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d: %v (in %q)", e.Line, e.Err, e.Text)
}

func (e *ParseError) Unwrap() error { return e.Err }

// LoadFile reads and parses an endpoints file from disk.
func LoadFile(path string, def Defaults) ([]model.Target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, def)
}

// Parse reads targets from r. Disabled targets are returned too, with
// Enabled=false, so that `stats` and the TUI can still describe them.
func Parse(r io.Reader, def Defaults) ([]model.Target, error) {
	var out []model.Target
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	seen := map[string]int{} // name -> count, for de-duplicating display names
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}

		fields, err := tokenize(line)
		if err != nil {
			return nil, &ParseError{Line: lineNo, Text: raw, Err: err}
		}
		if len(fields) == 0 {
			continue
		}

		// A `defaults` line mutates the baseline for everything below it.
		if fields[0] == "defaults" {
			if err := applyDefaults(&def, fields[1:]); err != nil {
				return nil, &ParseError{Line: lineNo, Text: raw, Err: err}
			}
			continue
		}

		t, err := buildTarget(fields[0], fields[1:], def)
		if err != nil {
			return nil, &ParseError{Line: lineNo, Text: raw, Err: err}
		}

		// Uptime Kuma allows duplicate names; line protocol tags are a key, so
		// we disambiguate rather than silently merging two series. The suffix
		// search skips names already taken, including ones a user wrote by
		// hand, so three "dup" lines become dup, dup#2, dup#3.
		base := t.Name
		if seen[base] > 0 {
			for i := seen[base] + 1; ; i++ {
				cand := fmt.Sprintf("%s#%d", base, i)
				if seen[cand] == 0 {
					t.Name = cand
					break
				}
			}
			seen[t.Name]++
		}
		seen[base]++

		out = append(out, t)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no targets found")
	}
	return out, nil
}

// stripComment removes a trailing `#` comment, respecting quotes and
// backslash escapes so that a URL fragment or a keyword containing '#'
// survives.
func stripComment(s string) string {
	var q rune
	esc := false
	for i, r := range s {
		switch {
		case esc:
			esc = false
		case r == '\\':
			esc = true
		case q != 0:
			if r == q {
				q = 0
			}
		case r == '\'' || r == '"':
			q = r
		case r == '#':
			// Only a comment when at line start or preceded by whitespace.
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return s[:i]
			}
		}
	}
	return s
}

// tokenize splits on whitespace with support for quoted runs. A backslash
// escapes the next character anywhere, which is what lets an imported keyword
// contain both quote styles without needing a different file format.
func tokenize(s string) ([]string, error) {
	var (
		out []string
		cur strings.Builder
		q   rune
		has bool
		esc bool
	)
	flush := func() {
		if has {
			out = append(out, cur.String())
			cur.Reset()
			has = false
		}
	}
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			has = true
			esc = false
		case r == '\\':
			esc = true
		case q != 0:
			if r == q {
				q = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			q = r
			has = true // an empty quoted string is still a token
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if esc {
		return nil, fmt.Errorf("trailing backslash")
	}
	if q != 0 {
		return nil, fmt.Errorf("unterminated %c quote", q)
	}
	flush()
	return out, nil
}

// Quote renders v so that tokenize reads it back unchanged. Bare words stay
// bare, which keeps a generated endpoints.txt readable by hand.
func Quote(v string) string {
	if v == "" {
		return `""`
	}
	if !strings.ContainsAny(v, " \t\"'\\#") {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// splitOpt splits "key=value"; a bare "key" is treated as "key=true" so that
// boolean flags can be written without a value.
func splitOpt(tok string) (string, string) {
	k, v, ok := strings.Cut(tok, "=")
	if !ok {
		return strings.ToLower(k), "true"
	}
	return strings.ToLower(k), v
}

func applyDefaults(def *Defaults, opts []string) error {
	for _, tok := range opts {
		if err := parseInto(nil, def, tok); err != nil {
			return err
		}
	}
	return nil
}

// parseInto applies one option token to a target (when t != nil) and/or to the
// defaults (when d != nil). Keeping both in one switch means the `defaults`
// line and a per-target override can never drift apart.
func parseInto(t *model.Target, d *Defaults, tok string) error {
	k, v := splitOpt(tok)

	dur := func() (time.Duration, error) { return parseDuration(v) }
	boolean := func() (bool, error) { return strconv.ParseBool(v) }

	switch k {
	case "name":
		if t == nil {
			return fmt.Errorf("name is not valid on a defaults line")
		}
		t.Name = v
	case "group":
		if t != nil {
			t.Group = v
		} else {
			d.Group = v
		}
	case "interval":
		x, err := dur()
		if err != nil {
			return err
		}
		if x <= 0 {
			return fmt.Errorf("interval must be positive")
		}
		if t != nil {
			t.Interval = x
		} else {
			d.Interval = x
		}
	case "timeout":
		x, err := dur()
		if err != nil {
			return err
		}
		if x <= 0 {
			return fmt.Errorf("timeout must be positive")
		}
		if t != nil {
			t.Timeout = x
		} else {
			d.Timeout = x
		}
	case "retries":
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("retries must be a non-negative integer")
		}
		if t != nil {
			t.Retries = n
		} else {
			d.Retries = n
		}
	case "retry_interval", "retry-interval":
		x, err := dur()
		if err != nil {
			return err
		}
		if t != nil {
			t.RetryInterval = x
		} else {
			d.RetryInterval = x
		}
	case "redirects", "max_redirects", "max-redirects":
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("redirects must be a non-negative integer")
		}
		if t != nil {
			t.MaxRedirects = n
		} else {
			d.MaxRedirects = n
		}
	case "insecure":
		b, err := boolean()
		if err != nil {
			return err
		}
		if t != nil {
			t.Insecure = b
		} else {
			d.Insecure = b
		}
	case "ua", "user_agent", "user-agent":
		if t != nil {
			t.UserAgent = v
		} else {
			d.UserAgent = v
		}
	case "max_body", "max-body":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("max_body must be a non-negative integer")
		}
		if t != nil {
			t.MaxBody = n
		} else {
			d.MaxBody = n
		}
	case "expect", "accept", "status":
		rs, err := model.ParseStatusRanges(v)
		if err != nil {
			return err
		}
		if t != nil {
			t.Accept = rs
		} else {
			d.Accept = rs
		}

	// Target-only options below.
	case "keyword":
		if t == nil {
			return fmt.Errorf("keyword is not valid on a defaults line")
		}
		t.Keyword = v
		t.Kind = model.KindKeyword
	case "invert", "invert_keyword", "invert-keyword":
		if t == nil {
			return fmt.Errorf("invert is not valid on a defaults line")
		}
		b, err := boolean()
		if err != nil {
			return err
		}
		t.InvertKeyword = b
	case "method":
		if t == nil {
			return fmt.Errorf("method is not valid on a defaults line")
		}
		t.Method = strings.ToUpper(v)
	case "header":
		if t == nil {
			return fmt.Errorf("header is not valid on a defaults line")
		}
		hk, hv, ok := strings.Cut(v, ":")
		if !ok {
			return fmt.Errorf("header must be \"Name: value\"")
		}
		t.Headers = append(t.Headers, model.Header{
			Key:   strings.TrimSpace(hk),
			Value: strings.TrimSpace(hv),
		})
	case "body":
		if t == nil {
			return fmt.Errorf("body is not valid on a defaults line")
		}
		t.Body = v
	case "upside_down", "upside-down", "invert_result":
		if t == nil {
			return fmt.Errorf("upside_down is not valid on a defaults line")
		}
		b, err := boolean()
		if err != nil {
			return err
		}
		t.UpsideDown = b
	case "enabled", "active":
		if t == nil {
			return fmt.Errorf("enabled is not valid on a defaults line")
		}
		b, err := boolean()
		if err != nil {
			return err
		}
		t.Enabled = b
	case "disabled":
		if t == nil {
			return fmt.Errorf("disabled is not valid on a defaults line")
		}
		b, err := boolean()
		if err != nil {
			return err
		}
		t.Enabled = !b
	default:
		return fmt.Errorf("unknown option %q", k)
	}
	return nil
}

func buildTarget(target string, opts []string, def Defaults) (model.Target, error) {
	t := model.Target{
		Method:        "GET",
		Accept:        def.Accept,
		Interval:      def.Interval,
		Timeout:       def.Timeout,
		Retries:       def.Retries,
		RetryInterval: def.RetryInterval,
		MaxRedirects:  def.MaxRedirects,
		Insecure:      def.Insecure,
		UserAgent:     def.UserAgent,
		MaxBody:       def.MaxBody,
		Group:         def.Group,
		Enabled:       true,
	}

	switch {
	case strings.HasPrefix(target, "ping://"):
		t.Kind = model.KindPing
		t.Host = strings.TrimPrefix(target, "ping://")
		t.Host = strings.TrimSuffix(t.Host, "/")
		if t.Host == "" {
			return t, fmt.Errorf("ping target needs a host")
		}
		t.Name = "ping " + t.Host

	case strings.HasPrefix(target, "tcp://"):
		t.Kind = model.KindTCP
		hostport := strings.TrimPrefix(target, "tcp://")
		host, portStr, ok := strings.Cut(hostport, ":")
		if !ok {
			return t, fmt.Errorf("tcp target needs host:port")
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return t, fmt.Errorf("bad tcp port %q", portStr)
		}
		t.Host, t.Port = host, p
		t.Name = fmt.Sprintf("tcp %s:%d", host, p)

	default:
		// Bare hostnames are a common paste; assume https.
		if !strings.Contains(target, "://") {
			target = "https://" + target
		}
		u, err := url.Parse(target)
		if err != nil {
			return t, fmt.Errorf("bad url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return t, fmt.Errorf("unsupported scheme %q", u.Scheme)
		}
		if u.Host == "" {
			return t, fmt.Errorf("url has no host")
		}
		t.Kind = model.KindHTTP
		t.URL = u.String()
		t.Host = u.Hostname()
		t.Name = t.URL
	}

	for _, tok := range opts {
		if err := parseInto(&t, nil, tok); err != nil {
			return t, err
		}
	}

	// A keyword on a ping/tcp target is meaningless; catch it rather than
	// silently ignoring the user's intent.
	if t.Keyword != "" && t.Kind != model.KindKeyword && t.Kind != model.KindHTTP {
		return t, fmt.Errorf("keyword is only valid for http targets")
	}
	if t.Name == "" {
		t.Name = target
	}
	return t, nil
}

// parseDuration accepts Go duration strings and bare numbers, where a bare
// number means seconds. Uptime Kuma stores intervals as integer seconds, so
// this keeps imported files readable.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

// Filter narrows a target list by group and name substring, and optionally
// drops disabled entries. Used by every subcommand that takes -group/-endpoint.
func Filter(in []model.Target, group, nameSubstr string, enabledOnly bool) []model.Target {
	var out []model.Target
	for _, t := range in {
		if enabledOnly && !t.Enabled {
			continue
		}
		if group != "" && !strings.EqualFold(t.Group, group) {
			continue
		}
		if nameSubstr != "" && !strings.Contains(strings.ToLower(t.Name), strings.ToLower(nameSubstr)) {
			continue
		}
		out = append(out, t)
	}
	return out
}

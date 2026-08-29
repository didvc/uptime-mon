// Package importer converts an Uptime Kuma backup export into an
// endpoints.txt file.
//
// The conversion is deliberately lossy. Kuma's monitor record carries about
// eighty fields covering Docker, MQTT, gRPC, Radius, Kafka, game servers and
// half a dozen notification integrations; this tool probes HTTP, TCP and ICMP.
// Anything outside that is reported as a warning and skipped, rather than
// silently dropped or half-translated into something that would look like it
// works.
package importer

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// Options controls which monitors survive the conversion.
type Options struct {
	// IncludeInactive keeps monitors Kuma has paused, written out with
	// enabled=false so they are documented but not probed.
	IncludeInactive bool
	// OnlyTypes, when non-empty, restricts to these Kuma monitor types.
	OnlyTypes []string
	// GroupFrom picks where the group tag comes from: "tag" (default),
	// "parent" (Kuma's monitor group), or "none".
	GroupFrom string
	// DefaultGroup is used when the chosen source yields nothing.
	DefaultGroup string
}

// Warning is a monitor that could not be converted faithfully.
type Warning struct {
	Monitor string
	Reason  string
}

func (w Warning) String() string { return fmt.Sprintf("%s: %s", w.Monitor, w.Reason) }

// kumaBackup is the subset of the export we read.
type kumaBackup struct {
	Version     string        `json:"version"`
	MonitorList []kumaMonitor `json:"monitorList"`
}

type kumaMonitor struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Method   string `json:"method"`
	Hostname string `json:"hostname"`
	Port     *int   `json:"port"`
	Active   bool   `json:"active"`

	Interval      float64 `json:"interval"`
	Timeout       float64 `json:"timeout"`
	MaxRetries    int     `json:"maxretries"`
	RetryInterval float64 `json:"retryInterval"`
	MaxRedirects  *int    `json:"maxredirects"`

	Keyword       string `json:"keyword"`
	InvertKeyword bool   `json:"invertKeyword"`

	AcceptedStatusCodes []string `json:"accepted_statuscodes"`
	IgnoreTLS           bool     `json:"ignoreTls"`
	UpsideDown          bool     `json:"upsideDown"`

	Headers *string `json:"headers"`
	Body    *string `json:"body"`

	BasicAuthUser *string `json:"basic_auth_user"`
	BasicAuthPass *string `json:"basic_auth_pass"`

	Parent   *int   `json:"parent"`
	PathName string `json:"pathName"`

	Tags []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"tags"`
}

// LoadFile reads and converts a Kuma backup.
func LoadFile(path string, opts Options) ([]model.Target, []Warning, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return Load(f, opts)
}

// Load converts a Kuma backup read from r.
func Load(r io.Reader, opts Options) ([]model.Target, []Warning, error) {
	var b kumaBackup
	dec := json.NewDecoder(r)
	if err := dec.Decode(&b); err != nil {
		return nil, nil, fmt.Errorf("parse kuma backup: %w", err)
	}
	if len(b.MonitorList) == 0 {
		return nil, nil, fmt.Errorf("backup contains no monitors")
	}

	only := map[string]bool{}
	for _, t := range opts.OnlyTypes {
		only[strings.ToLower(strings.TrimSpace(t))] = true
	}

	// Kuma nests monitors under group monitors; resolve parent names so a
	// group can become our flat `group=` tag.
	nameByID := make(map[int]string, len(b.MonitorList))
	for _, m := range b.MonitorList {
		nameByID[m.ID] = m.Name
	}

	var (
		targets  []model.Target
		warnings []Warning
	)
	for _, m := range b.MonitorList {
		if len(only) > 0 && !only[strings.ToLower(m.Type)] {
			continue
		}
		if !m.Active && !opts.IncludeInactive {
			continue
		}
		t, warn := convert(m, nameByID, opts)
		if warn != nil {
			warnings = append(warnings, *warn)
			continue
		}
		targets = append(targets, t)
	}
	if len(targets) == 0 {
		return nil, warnings, fmt.Errorf("no convertible monitors found")
	}
	return targets, warnings, nil
}

func convert(m kumaMonitor, nameByID map[int]string, opts Options) (model.Target, *Warning) {
	t := model.Target{
		Name:          m.Name,
		Method:        strings.ToUpper(strings.TrimSpace(m.Method)),
		Enabled:       m.Active,
		InvertKeyword: m.InvertKeyword,
		Insecure:      m.IgnoreTLS,
		UpsideDown:    m.UpsideDown,
		Retries:       m.MaxRetries,
		MaxBody:       1 << 20,
	}
	if t.Method == "" {
		t.Method = "GET"
	}

	switch strings.ToLower(m.Type) {
	case "http":
		t.Kind = model.KindHTTP
		t.URL = m.URL
	case "keyword":
		t.Kind = model.KindKeyword
		t.URL = m.URL
		t.Keyword = m.Keyword
		if t.Keyword == "" {
			return t, &Warning{m.Name, "keyword monitor has no keyword"}
		}
	case "ping":
		t.Kind = model.KindPing
		t.Host = m.Hostname
		if t.Host == "" {
			return t, &Warning{m.Name, "ping monitor has no hostname"}
		}
	case "port", "tcp":
		if m.Port == nil || *m.Port <= 0 {
			return t, &Warning{m.Name, "tcp monitor has no port"}
		}
		t.Kind = model.KindTCP
		t.Host = m.Hostname
		t.Port = *m.Port
	default:
		return t, &Warning{m.Name, fmt.Sprintf("monitor type %q is not supported", m.Type)}
	}

	if t.Kind == model.KindHTTP || t.Kind == model.KindKeyword {
		if strings.TrimSpace(m.URL) == "" || m.URL == "https://" {
			return t, &Warning{m.Name, "monitor has no URL"}
		}
		t.URL = m.URL
		if u := hostOf(m.URL); u != "" {
			t.Host = u
		}
	}

	// Kuma stores seconds, sometimes fractional.
	t.Interval = secs(m.Interval, 60*time.Second)
	t.Timeout = secs(m.Timeout, 10*time.Second)
	t.RetryInterval = secs(m.RetryInterval, 20*time.Second)
	// Kuma's default timeout is 48s against a 60s interval, which is a very
	// long time to wait for a dead endpoint. Clamp it below the interval so a
	// hung probe cannot overlap the next one.
	if t.Interval > 0 && t.Timeout >= t.Interval {
		t.Timeout = t.Interval - t.Interval/10
	}

	t.MaxRedirects = 10
	if m.MaxRedirects != nil {
		t.MaxRedirects = *m.MaxRedirects
	}

	acc, err := model.ParseStatusRanges(strings.Join(m.AcceptedStatusCodes, ","))
	if err != nil {
		return t, &Warning{m.Name, "unparseable accepted status codes: " + err.Error()}
	}
	t.Accept = acc

	// Headers arrive as a JSON object encoded in a string field.
	if m.Headers != nil && strings.TrimSpace(*m.Headers) != "" {
		hs, herr := parseHeaders(*m.Headers)
		if herr != nil {
			return t, &Warning{m.Name, "unparseable headers: " + herr.Error()}
		}
		t.Headers = hs
	}
	if m.Body != nil {
		t.Body = strings.TrimSpace(*m.Body)
	}
	if m.BasicAuthUser != nil && *m.BasicAuthUser != "" {
		pass := ""
		if m.BasicAuthPass != nil {
			pass = *m.BasicAuthPass
		}
		t.Headers = append(t.Headers, model.Header{
			Key:   "Authorization",
			Value: "Basic " + basicAuth(*m.BasicAuthUser, pass),
		})
	}

	t.Group = groupFor(m, nameByID, opts)
	return t, nil
}

func groupFor(m kumaMonitor, nameByID map[int]string, opts Options) string {
	switch opts.GroupFrom {
	case "none":
		return opts.DefaultGroup
	case "parent":
		if m.Parent != nil {
			if n, ok := nameByID[*m.Parent]; ok && n != "" {
				return n
			}
		}
	default: // "tag"
		if len(m.Tags) > 0 && m.Tags[0].Name != "" {
			return m.Tags[0].Name
		}
		if m.Parent != nil {
			if n, ok := nameByID[*m.Parent]; ok && n != "" {
				return n
			}
		}
	}
	return opts.DefaultGroup
}

func parseHeaders(s string) ([]model.Header, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	out := make([]model.Header, 0, len(raw))
	for k, v := range raw {
		out = append(out, model.Header{Key: k, Value: fmt.Sprint(v)})
	}
	// Map iteration is random; sort so repeated imports produce identical files.
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func basicAuth(user, pass string) string {
	const b64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := []byte(user + ":" + pass)
	var b strings.Builder
	for i := 0; i < len(src); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], src[i:])
		b.WriteByte(b64[chunk[0]>>2])
		b.WriteByte(b64[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			b.WriteByte(b64[(chunk[1]&0x0f)<<2|chunk[2]>>6])
		} else {
			b.WriteByte('=')
		}
		if n > 2 {
			b.WriteByte(b64[chunk[2]&0x3f])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

func secs(v float64, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return time.Duration(v * float64(time.Second))
}

func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if strings.HasPrefix(s, "[") { // IPv6 literal
		if i := strings.Index(s, "]"); i >= 0 {
			return s[1:i]
		}
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

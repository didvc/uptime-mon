package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/didvc/uptime-mon/internal/lineproto"
	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/stats"
	"github.com/didvc/uptime-mon/internal/store"
)

func cmdStats(args []string) error {
	fs := newFlagSet("stats", "Summarise recorded samples over a time window.\n\n"+
		"Reads the day files directly, including compressed ones, so it can be\n"+
		"run against a directory while a collector is writing to it.")

	dir := fs.String("data", "./data", "data directory")
	prefix := fs.String("prefix", "uptime", "day-file prefix")
	precision := fs.String("precision", "ns", "timestamp precision the files were written with")
	window := fs.Duration("window", 24*time.Hour, "window ending at -until")
	since := fs.String("since", "", "start of the window (24h, 7d, 2026-08-29, RFC3339); overrides -window")
	until := fs.String("until", "now", "end of the window")
	endpoint := fs.String("endpoint", "", "only endpoints whose name contains this substring")
	group := fs.String("group", "", "only endpoints in this group")
	sortKey := fs.String("sort", "availability", "sort by: name, availability, rtt, p95, outages, apdex, samples, group")
	format := fs.String("format", "table", "output format: table, detail, json, csv or prom")
	apdexT := fs.Duration("apdex", 500*time.Millisecond, "Apdex target latency T")
	top := fs.Int("top", 0, "show only the first N endpoints after sorting")
	buckets := fs.Int("buckets", 0, "compute this many time buckets (needed for -format json charts)")
	noColor := fs.Bool("no-color", false, "disable ANSI colour")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prec, err := lineproto.ParsePrecision(*precision)
	if err != nil {
		return err
	}

	now := time.Now()
	to, err := parseWhen(*until, now)
	if err != nil {
		return err
	}
	from := to.Add(-*window)
	if *since != "" {
		if from, err = parseWhen(*since, now); err != nil {
			return err
		}
	}
	if !from.Before(to) {
		return fmt.Errorf("empty window: %s is not before %s",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	r := &store.Reader{Dir: *dir, Prefix: *prefix, Precision: prec}
	acc := stats.NewAccumulator(from, to, *buckets, *apdexT)
	scanned := 0
	err = r.Scan(from, to, func(res model.Result) error {
		if *endpoint != "" && !strings.Contains(strings.ToLower(res.Target), strings.ToLower(*endpoint)) {
			return nil
		}
		if *group != "" && !strings.EqualFold(res.Group, *group) {
			return nil
		}
		scanned++
		acc.Add(res)
		return nil
	})
	if err != nil {
		return err
	}
	if scanned == 0 {
		return fmt.Errorf("no samples in %s .. %s (is -data %q right?)",
			from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"), *dir)
	}

	sums := acc.Summaries()
	stats.SortSummaries(sums, *sortKey)
	// Keep the real count: -top narrows what is printed, not what was measured.
	totalEndpoints := len(sums)
	if *top > 0 && *top < len(sums) {
		sums = sums[:*top]
	}
	overall := acc.Overall()

	switch *format {
	case "json":
		return emitJSON(sums, overall, from, to)
	case "csv":
		return emitCSV(sums)
	case "prom":
		return emitProm(sums)
	case "detail":
		return emitDetail(sums, overall, from, to, colorEnabled(*noColor, true))
	case "table":
		return emitTable(sums, overall, totalEndpoints, from, to, colorEnabled(*noColor, true))
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
}

// ---------------------------------------------------------------------------

func emitTable(sums []stats.Summary, overall stats.Summary, total int, from, to time.Time, color bool) error {
	c := colorizer{color}

	fmt.Printf("%s .. %s  (%s)\n",
		from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"),
		shortDur(to.Sub(from)))
	shown := ""
	if len(sums) != total {
		shown = fmt.Sprintf(" (showing %d)", len(sums))
	}
	fmt.Printf("%d endpoints%s  %s samples  avail %s  p50 %s  p95 %s  p99 %s  apdex %.3f\n\n",
		total, shown, thousands(overall.Samples),
		c.health(overall.Availability, fmtPct(overall.Availability)),
		fmtMS(overall.RTT.Median), fmtMS(overall.RTT.P95), fmtMS(overall.RTT.P99),
		overall.Apdex)

	nameW := 24
	for _, s := range sums {
		if n := len(s.Target); n > nameW {
			nameW = n
		}
	}
	if nameW > 56 {
		nameW = 56
	}

	fmt.Printf("%-*s %6s %5s %8s %8s %8s %8s %6s %5s %8s\n",
		nameW, "ENDPOINT", "STATE", "N", "AVAIL", "P50", "P95", "P99", "OUT", "APDEX", "DOWNTIME")
	fmt.Println(strings.Repeat("-", nameW+1+6+1+5+1+8*4+4+6+1+5+1+8))

	for _, s := range sums {
		state := c.green("up")
		if s.Samples == 0 {
			state = c.grey("?")
		} else if s.LastStatus != model.StatusUp {
			state = c.red("DOWN")
		}
		fmt.Printf("%-*s %6s %5s %8s %8s %8s %8s %6d %5.3f %8s\n",
			nameW, truncPlain(s.Target, nameW),
			state,
			thousands(s.Samples),
			c.health(s.Availability, fmtPct(s.Availability)),
			fmtMS(s.RTT.Median), fmtMS(s.RTT.P95), fmtMS(s.RTT.P99),
			s.OutageCount, s.Apdex, shortDur(s.Downtime))
	}
	return nil
}

func emitDetail(sums []stats.Summary, overall stats.Summary, from, to time.Time, color bool) error {
	c := colorizer{color}
	fmt.Printf("%s .. %s  (%s)\n\n",
		from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"), shortDur(to.Sub(from)))

	for _, s := range sums {
		head := s.Target
		if s.Group != "" {
			head = s.Group + "/" + head
		}
		fmt.Println(c.bold(head))

		state := "up"
		if s.Samples == 0 {
			state = "no samples"
		} else if s.LastStatus != model.StatusUp {
			state = "DOWN: " + s.LastErr
		}
		fmt.Printf("  state      %s %s (last seen %s)\n",
			state, streakDesc(s.StreakLength), s.LastSeen.Format("2006-01-02 15:04:05"))
		fmt.Printf("  avail      %s of %s samples   %d up / %d down   interval %s\n",
			fmtPct(s.Availability), thousands(s.Samples), s.Up, s.Down, shortDur(s.Interval))
		fmt.Printf("  latency    min %s  p25 %s  p50 %s  p75 %s  p90 %s  p95 %s  p99 %s  max %s\n",
			fmtMS(s.RTT.Min), fmtMS(s.RTT.P25), fmtMS(s.RTT.Median), fmtMS(s.RTT.P75),
			fmtMS(s.RTT.P90), fmtMS(s.RTT.P95), fmtMS(s.RTT.P99), fmtMS(s.RTT.Max))
		fmt.Printf("  spread     mean %s  stddev %s  MAD %s  IQR %s  CV %.2f  skew %.2f\n",
			fmtMS(s.RTT.Mean), fmtMS(s.RTT.StdDev), fmtMS(s.RTT.MAD), fmtMS(s.RTT.IQR),
			s.RTT.CV, s.RTT.Skew)
		fmt.Printf("  outages    %d   downtime %s   longest %s   MTTR %s   MTBF %s\n",
			s.OutageCount, shortDur(s.Downtime), shortDur(s.LongestOutage),
			shortDur(s.MTTR), shortDur(s.MTBF))
		fmt.Printf("  apdex      %.3f (T=%s)", s.Apdex, shortDur(s.ApdexT))
		if s.FailRTT.N > 0 {
			fmt.Printf("   time-to-fail median %s", fmtMS(s.FailRTT.Median))
		}
		fmt.Println()
		if s.Gaps > 0 {
			fmt.Printf("  gaps       %d, totalling %s unmonitored\n", s.Gaps, shortDur(s.GapDuration))
		}
		if len(s.Errors) > 0 {
			parts := make([]string, 0, len(s.Errors))
			for i, e := range s.Errors {
				if i == 5 {
					parts = append(parts, fmt.Sprintf("+%d more", len(s.Errors)-i))
					break
				}
				parts = append(parts, fmt.Sprintf("%s x%d", e.Err, e.Count))
			}
			fmt.Printf("  failures   %s\n", strings.Join(parts, "; "))
		}
		if len(s.Codes) > 0 {
			fmt.Printf("  codes      %s\n", formatCodes(s.Codes))
		}
		fmt.Println()
	}

	fmt.Printf("%s  avail %s  p50 %s  p95 %s  apdex %.3f over %s samples\n",
		c.bold("ALL"), fmtPct(overall.Availability),
		fmtMS(overall.RTT.Median), fmtMS(overall.RTT.P95),
		overall.Apdex, thousands(overall.Samples))
	return nil
}

// jsonSummary is the wire shape. It is written by hand rather than tagging
// stats.Summary so the output stays stable if the internal struct changes.
type jsonSummary struct {
	Endpoint     string         `json:"endpoint"`
	Group        string         `json:"group,omitempty"`
	Host         string         `json:"host,omitempty"`
	Kind         string         `json:"kind,omitempty"`
	Samples      int            `json:"samples"`
	Up           int            `json:"up"`
	Down         int            `json:"down"`
	Availability float64        `json:"availability"`
	Nines        *float64       `json:"nines,omitempty"`
	LastStatus   string         `json:"last_status"`
	LastSeen     *time.Time     `json:"last_seen,omitempty"`
	LastError    string         `json:"last_error,omitempty"`
	StreakSecs   float64        `json:"streak_seconds"`
	RTT          jsonDist       `json:"rtt_ms"`
	FailRTT      jsonDist       `json:"fail_rtt_ms"`
	Outages      int            `json:"outages"`
	DowntimeSecs float64        `json:"downtime_seconds"`
	LongestSecs  float64        `json:"longest_outage_seconds"`
	MTTRSecs     float64        `json:"mttr_seconds"`
	MTBFSecs     float64        `json:"mtbf_seconds"`
	IntervalSecs float64        `json:"interval_seconds"`
	Gaps         int            `json:"gaps"`
	Apdex        float64        `json:"apdex"`
	ApdexT       float64        `json:"apdex_t_seconds"`
	Errors       map[string]int `json:"errors,omitempty"`
	Codes        map[int]int    `json:"status_codes,omitempty"`
	Buckets      []jsonBucket   `json:"buckets,omitempty"`
}

type jsonDist struct {
	N      int     `json:"n"`
	Min    float64 `json:"min"`
	P25    float64 `json:"p25"`
	Median float64 `json:"median"`
	P75    float64 `json:"p75"`
	P90    float64 `json:"p90"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
	MAD    float64 `json:"mad"`
	IQR    float64 `json:"iqr"`
	CV     float64 `json:"cv"`
	Skew   float64 `json:"skew"`
}

type jsonBucket struct {
	Start        time.Time `json:"start"`
	Samples      int       `json:"samples"`
	Up           int       `json:"up"`
	Availability *float64  `json:"availability"`
	MeanRTT      float64   `json:"mean_rtt_ms"`
	P95RTT       float64   `json:"p95_rtt_ms"`
}

func toJSONDist(d stats.Dist) jsonDist {
	return jsonDist{
		N: d.N, Min: d.Min, P25: d.P25, Median: d.Median, P75: d.P75,
		P90: d.P90, P95: d.P95, P99: d.P99, Max: d.Max, Mean: d.Mean,
		StdDev: d.StdDev, MAD: d.MAD, IQR: d.IQR, CV: d.CV, Skew: d.Skew,
	}
}

func toJSONSummary(s stats.Summary) jsonSummary {
	j := jsonSummary{
		Endpoint: s.Target, Group: s.Group, Host: s.Host, Kind: string(s.Kind),
		Samples: s.Samples, Up: s.Up, Down: s.Down,
		Availability: s.Availability,
		LastStatus:   s.LastStatus.String(),
		LastError:    s.LastErr,
		StreakSecs:   s.StreakLength.Seconds(),
		RTT:          toJSONDist(s.RTT),
		FailRTT:      toJSONDist(s.FailRTT),
		Outages:      s.OutageCount,
		DowntimeSecs: s.Downtime.Seconds(),
		LongestSecs:  s.LongestOutage.Seconds(),
		MTTRSecs:     s.MTTR.Seconds(),
		MTBFSecs:     s.MTBF.Seconds(),
		IntervalSecs: s.Interval.Seconds(),
		Gaps:         s.Gaps,
		Apdex:        s.Apdex,
		ApdexT:       s.ApdexT.Seconds(),
		Codes:        s.Codes,
	}
	// +Inf is not valid JSON, so perfect availability omits the field rather
	// than emitting something a parser will choke on.
	if !math.IsInf(s.Nines, 1) {
		n := s.Nines
		j.Nines = &n
	}
	if s.Samples > 0 {
		t := s.LastSeen
		j.LastSeen = &t
	}
	if len(s.Errors) > 0 {
		j.Errors = make(map[string]int, len(s.Errors))
		for _, e := range s.Errors {
			j.Errors[e.Err] = e.Count
		}
	}
	for _, b := range s.Buckets {
		jb := jsonBucket{Start: b.Start, Samples: b.Samples, Up: b.Up,
			MeanRTT: b.MeanRTT, P95RTT: b.P95RTT}
		if av := b.Availability(); !math.IsNaN(av) {
			jb.Availability = &av
		}
		j.Buckets = append(j.Buckets, jb)
	}
	return j
}

func emitJSON(sums []stats.Summary, overall stats.Summary, from, to time.Time) error {
	out := struct {
		From      time.Time     `json:"from"`
		To        time.Time     `json:"to"`
		Overall   jsonSummary   `json:"overall"`
		Endpoints []jsonSummary `json:"endpoints"`
	}{From: from, To: to, Overall: toJSONSummary(overall)}
	for _, s := range sums {
		out.Endpoints = append(out.Endpoints, toJSONSummary(s))
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func emitCSV(sums []stats.Summary) error {
	w := csv.NewWriter(os.Stdout)
	defer w.Flush()

	if err := w.Write([]string{
		"endpoint", "group", "host", "kind", "samples", "up", "down",
		"availability", "last_status", "rtt_min", "rtt_p50", "rtt_p95", "rtt_p99",
		"rtt_max", "rtt_mean", "rtt_stddev", "rtt_mad", "rtt_iqr", "rtt_cv",
		"outages", "downtime_s", "mttr_s", "mtbf_s", "apdex", "interval_s", "gaps",
	}); err != nil {
		return err
	}
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
	for _, s := range sums {
		if err := w.Write([]string{
			s.Target, s.Group, s.Host, string(s.Kind),
			strconv.Itoa(s.Samples), strconv.Itoa(s.Up), strconv.Itoa(s.Down),
			f(s.Availability), s.LastStatus.String(),
			f(s.RTT.Min), f(s.RTT.Median), f(s.RTT.P95), f(s.RTT.P99),
			f(s.RTT.Max), f(s.RTT.Mean), f(s.RTT.StdDev), f(s.RTT.MAD),
			f(s.RTT.IQR), f(s.RTT.CV),
			strconv.Itoa(s.OutageCount), f(s.Downtime.Seconds()),
			f(s.MTTR.Seconds()), f(s.MTBF.Seconds()), f(s.Apdex),
			f(s.Interval.Seconds()), strconv.Itoa(s.Gaps),
		}); err != nil {
			return err
		}
	}
	return w.Error()
}

// emitProm writes the Prometheus text exposition format, for anyone who wants
// to scrape a summary from cron without this tool growing an HTTP server.
func emitProm(sums []stats.Summary) error {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return strings.ReplaceAll(s, "\n", " ")
	}
	metrics := []struct {
		name, help, typ string
		get             func(stats.Summary) float64
	}{
		{"uptime_mon_availability_ratio", "Fraction of probes that succeeded.", "gauge",
			func(s stats.Summary) float64 { return s.Availability }},
		{"uptime_mon_samples_total", "Probes recorded in the window.", "gauge",
			func(s stats.Summary) float64 { return float64(s.Samples) }},
		{"uptime_mon_up", "1 if the last probe succeeded.", "gauge",
			func(s stats.Summary) float64 {
				if s.Samples > 0 && s.LastStatus == model.StatusUp {
					return 1
				}
				return 0
			}},
		{"uptime_mon_rtt_p50_milliseconds", "Median latency of successful probes.", "gauge",
			func(s stats.Summary) float64 { return s.RTT.Median }},
		{"uptime_mon_rtt_p95_milliseconds", "95th percentile latency.", "gauge",
			func(s stats.Summary) float64 { return s.RTT.P95 }},
		{"uptime_mon_rtt_p99_milliseconds", "99th percentile latency.", "gauge",
			func(s stats.Summary) float64 { return s.RTT.P99 }},
		{"uptime_mon_outages_total", "Distinct outages in the window.", "gauge",
			func(s stats.Summary) float64 { return float64(s.OutageCount) }},
		{"uptime_mon_downtime_seconds", "Summed outage duration.", "gauge",
			func(s stats.Summary) float64 { return s.Downtime.Seconds() }},
		{"uptime_mon_apdex", "Apdex score.", "gauge",
			func(s stats.Summary) float64 { return s.Apdex }},
	}
	for _, m := range metrics {
		fmt.Printf("# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
		for _, s := range sums {
			fmt.Printf("%s{endpoint=\"%s\",group=\"%s\",host=\"%s\",kind=\"%s\"} %g\n",
				m.name, esc(s.Target), esc(s.Group), esc(s.Host), esc(string(s.Kind)),
				m.get(s))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// small formatting helpers, shared by the non-TUI commands
// ---------------------------------------------------------------------------

type colorizer struct{ on bool }

func (c colorizer) wrap(code, s string) string {
	if !c.on {
		return s
	}
	return code + s + "\x1b[0m"
}
func (c colorizer) bold(s string) string  { return c.wrap("\x1b[1m", s) }
func (c colorizer) red(s string) string   { return c.wrap("\x1b[31m", s) }
func (c colorizer) green(s string) string { return c.wrap("\x1b[32m", s) }
func (c colorizer) grey(s string) string  { return c.wrap("\x1b[90m", s) }

func (c colorizer) health(avail float64, s string) string {
	switch {
	case avail >= 0.999:
		return c.green(s)
	case avail >= 0.99:
		return c.wrap("\x1b[33m", s)
	default:
		return c.red(s)
	}
}

func fmtPct(f float64) string {
	if math.IsNaN(f) {
		return "-"
	}
	switch {
	case f >= 1:
		return "100%"
	case f > 0.9999:
		return "99.99%"
	default:
		return fmt.Sprintf("%.2f%%", math.Floor(f*10000)/100)
	}
}

func fmtMS(v float64) string {
	switch {
	case v == 0:
		return "-"
	case v < 10:
		return fmt.Sprintf("%.1f", v)
	case v < 10000:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.1fs", v/1000)
	}
}

func shortDur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.0fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// streakDesc describes how long the current up/down run has lasted. A run
// that started with the most recent sample has zero length, which reads as
// "just now" rather than as a missing value.
func streakDesc(d time.Duration) string {
	if d <= 0 {
		return "as of the last probe"
	}
	return "for " + shortDur(d)
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncPlain(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func formatCodes(codes map[int]int) string {
	keys := make([]int, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d x%d", k, codes[k]))
	}
	return strings.Join(parts, "  ")
}

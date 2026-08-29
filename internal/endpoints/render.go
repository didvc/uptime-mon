package endpoints

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// Render writes targets as an endpoints.txt.
//
// The output is designed to be read and edited by a human afterwards: a
// `defaults` line carries whatever the majority of targets agree on, and each
// target line lists only what it does differently. Parsing the result back
// with Parse yields the same targets, which is what the round-trip test
// checks.
func Render(w io.Writer, targets []model.Target, header string) error {
	bw := bufio.NewWriter(w)

	if header != "" {
		for _, line := range strings.Split(strings.TrimRight(header, "\n"), "\n") {
			fmt.Fprintf(bw, "# %s\n", line)
		}
		fmt.Fprintln(bw)
	}

	def := deriveDefaults(targets)
	fmt.Fprintf(bw, "defaults interval=%s timeout=%s retries=%d retry_interval=%s redirects=%d expect=%s\n",
		dur(def.Interval), dur(def.Timeout), def.Retries,
		dur(def.RetryInterval), def.MaxRedirects, def.Accept)
	if def.Group != "" {
		fmt.Fprintf(bw, "defaults group=%s\n", Quote(def.Group))
	}
	if def.Insecure {
		fmt.Fprintln(bw, "defaults insecure=true")
	}
	fmt.Fprintln(bw)

	// Group targets under a comment header so a 90-line file has structure.
	byGroup := map[string][]model.Target{}
	var groupOrder []string
	for _, t := range targets {
		if _, ok := byGroup[t.Group]; !ok {
			groupOrder = append(groupOrder, t.Group)
		}
		byGroup[t.Group] = append(byGroup[t.Group], t)
	}
	sort.Strings(groupOrder)

	for _, g := range groupOrder {
		if len(groupOrder) > 1 {
			label := g
			if label == "" {
				label = "(ungrouped)"
			}
			fmt.Fprintf(bw, "# --- %s ---\n", label)
		}
		for _, t := range byGroup[g] {
			line, err := renderTarget(t, def)
			if err != nil {
				return err
			}
			fmt.Fprintln(bw, line)
		}
		fmt.Fprintln(bw)
	}
	return bw.Flush()
}

// deriveDefaults picks the most common value of each shared setting, so the
// defaults line covers as many targets as possible.
func deriveDefaults(targets []model.Target) Defaults {
	d := BaseDefaults()
	if len(targets) == 0 {
		return d
	}
	d.Interval = modeDur(targets, func(t model.Target) time.Duration { return t.Interval })
	d.Timeout = modeDur(targets, func(t model.Target) time.Duration { return t.Timeout })
	d.RetryInterval = modeDur(targets, func(t model.Target) time.Duration { return t.RetryInterval })
	d.Retries = modeInt(targets, func(t model.Target) int { return t.Retries })
	d.MaxRedirects = modeInt(targets, func(t model.Target) int { return t.MaxRedirects })
	d.Accept = model.StatusRanges(nil)
	if s := modeStr(targets, func(t model.Target) string { return t.Accept.String() }); s != "" {
		if rs, err := model.ParseStatusRanges(s); err == nil {
			d.Accept = rs
		}
	}
	if len(d.Accept) == 0 {
		d.Accept = model.DefaultAccept()
	}
	d.Group = modeStr(targets, func(t model.Target) string { return t.Group })
	d.Insecure = modeInt(targets, func(t model.Target) int {
		if t.Insecure {
			return 1
		}
		return 0
	}) == 1
	// UserAgent and MaxBody are not emitted per-target, so keep the baseline.
	base := BaseDefaults()
	d.UserAgent, d.MaxBody = base.UserAgent, base.MaxBody
	return d
}

func renderTarget(t model.Target, def Defaults) (string, error) {
	var spec, autoName string
	switch t.Kind {
	case model.KindPing:
		spec = "ping://" + t.Host
		autoName = "ping " + t.Host
	case model.KindTCP:
		spec = fmt.Sprintf("tcp://%s:%d", t.Host, t.Port)
		autoName = fmt.Sprintf("tcp %s:%d", t.Host, t.Port)
	case model.KindHTTP, model.KindKeyword:
		spec = t.URL
		autoName = t.URL
	default:
		return "", fmt.Errorf("cannot render target %q of kind %q", t.Name, t.Kind)
	}
	if strings.ContainsAny(spec, " \t") {
		return "", fmt.Errorf("target %q contains whitespace", spec)
	}

	var opts []string
	add := func(format string, args ...any) { opts = append(opts, fmt.Sprintf(format, args...)) }

	if t.Name != "" && t.Name != autoName {
		add("name=%s", Quote(t.Name))
	}
	if t.Group != def.Group {
		add("group=%s", Quote(t.Group))
	}
	if t.Interval != def.Interval {
		add("interval=%s", dur(t.Interval))
	}
	if t.Timeout != def.Timeout {
		add("timeout=%s", dur(t.Timeout))
	}
	if t.Retries != def.Retries {
		add("retries=%d", t.Retries)
	}
	if t.Retries > 0 && t.RetryInterval != def.RetryInterval {
		add("retry_interval=%s", dur(t.RetryInterval))
	}
	if t.MaxRedirects != def.MaxRedirects {
		add("redirects=%d", t.MaxRedirects)
	}
	if t.Accept.String() != def.Accept.String() {
		add("expect=%s", t.Accept)
	}
	if t.Insecure != def.Insecure {
		add("insecure=%t", t.Insecure)
	}
	if t.Method != "" && t.Method != "GET" {
		add("method=%s", t.Method)
	}
	if t.Keyword != "" {
		add("keyword=%s", Quote(t.Keyword))
		if t.InvertKeyword {
			add("invert=true")
		}
	}
	for _, h := range t.Headers {
		add("header=%s", Quote(h.Key+": "+h.Value))
	}
	if t.Body != "" {
		add("body=%s", Quote(t.Body))
	}
	if t.UpsideDown {
		add("upside_down=true")
	}
	if !t.Enabled {
		add("enabled=false")
	}

	if len(opts) == 0 {
		return spec, nil
	}
	// Pad the target column so options line up in the common case.
	pad := 44
	if len(spec) >= pad {
		return spec + " " + strings.Join(opts, " "), nil
	}
	return spec + strings.Repeat(" ", pad-len(spec)) + strings.Join(opts, " "), nil
}

// dur renders a duration the way a person would write it in the file.
func dur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Second == 0 {
		s := int64(d / time.Second)
		switch {
		case s%3600 == 0:
			return strconv.FormatInt(s/3600, 10) + "h"
		case s%60 == 0 && s >= 60:
			return strconv.FormatInt(s/60, 10) + "m"
		default:
			return strconv.FormatInt(s, 10) + "s"
		}
	}
	return d.String()
}

func modeDur(ts []model.Target, get func(model.Target) time.Duration) time.Duration {
	counts := map[time.Duration]int{}
	for _, t := range ts {
		counts[get(t)]++
	}
	var best time.Duration
	bestN := -1
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

func modeInt(ts []model.Target, get func(model.Target) int) int {
	counts := map[int]int{}
	for _, t := range ts {
		counts[get(t)]++
	}
	best, bestN := 0, -1
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

func modeStr(ts []model.Target, get func(model.Target) string) string {
	counts := map[string]int{}
	for _, t := range ts {
		counts[get(t)]++
	}
	best, bestN := "", -1
	for v, n := range counts {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

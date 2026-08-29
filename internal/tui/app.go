package tui

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/series"
	"github.com/didvc/uptime-mon/internal/stats"
)

// Options configures the live view.
type Options struct {
	// Set is the sample source. It is read under its own lock, so the
	// collector may keep writing to it while the TUI renders.
	Set *series.Set

	// Reload repopulates Set for a new window, by re-reading the day files.
	// Optional: without it, window changes are limited to samples already in
	// memory.
	Reload func(from, to time.Time) error

	// Title is shown in the header, typically the data directory.
	Title string

	Window  time.Duration // initial window, default 24h
	Refresh time.Duration // redraw period, default 1s
	ApdexT  time.Duration // Apdex target latency

	Color bool

	// Status supplies an extra fragment for the footer, e.g. write buffering.
	Status func() string
}

// windows are the presets bound to keys 1-5.
var windows = []struct {
	d     time.Duration
	label string
}{
	{time.Hour, "1h"},
	{6 * time.Hour, "6h"},
	{24 * time.Hour, "24h"},
	{7 * 24 * time.Hour, "7d"},
	{30 * 24 * time.Hour, "30d"},
}

var sortKeys = []string{"name", "availability", "p95", "rtt", "outages", "apdex", "group"}

type view int

const (
	viewList view = iota
	viewDetail
	viewHelp
)

type app struct {
	opts Options
	scr  *screen
	th   theme

	view     view
	prevView view

	windowIdx int
	sortIdx   int

	selName string
	scroll  int

	filtering bool
	filter    string

	paused bool
	notice string

	sums     []stats.Summary
	overall  stats.Summary
	lastCalc time.Time
	buckets  int
}

// Run takes over the terminal and renders until the context is cancelled or
// the user quits.
func Run(ctx context.Context, opts Options) error {
	if opts.Set == nil {
		return fmt.Errorf("tui: Set is required")
	}
	if opts.Window <= 0 {
		opts.Window = 24 * time.Hour
	}
	if opts.Refresh <= 0 {
		opts.Refresh = time.Second
	}
	if opts.ApdexT <= 0 {
		opts.ApdexT = 500 * time.Millisecond
	}

	a := &app{opts: opts, th: theme{on: opts.Color}, buckets: 60}
	a.windowIdx = 2 // 24h
	for i, w := range windows {
		if w.d == opts.Window {
			a.windowIdx = i
		}
	}

	a.scr = newScreen(opts.Color)
	if err := a.scr.enter(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	restore := &restoreOnce{fn: a.scr.leave}
	defer restore.do()

	// A terminal left in raw mode after a signal is a broken shell, so the
	// restore runs from the signal path too.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	done := make(chan struct{})
	defer close(done)
	keys := readKeys(done)

	ticker := time.NewTicker(opts.Refresh)
	defer ticker.Stop()

	a.recompute()
	if err := a.draw(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sigCh:
			return nil
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			if quit := a.handleKey(k); quit {
				return nil
			}
		case <-ticker.C:
			if !a.paused {
				a.recompute()
			}
		}
		if err := a.draw(); err != nil {
			return err
		}
	}
}

func (a *app) window() time.Duration { return windows[a.windowIdx].d }

func (a *app) recompute() {
	to := time.Now()
	from := to.Add(-a.window())
	a.sums = a.opts.Set.Summaries(from, to, a.buckets, a.opts.ApdexT)
	a.overall = a.opts.Set.Overall(from, to, a.opts.ApdexT)
	a.lastCalc = to
	stats.SortSummaries(a.sums, sortKeys[a.sortIdx])
}

// visible applies the name filter.
func (a *app) visible() []stats.Summary {
	if a.filter == "" {
		return a.sums
	}
	needle := strings.ToLower(a.filter)
	out := make([]stats.Summary, 0, len(a.sums))
	for _, s := range a.sums {
		if strings.Contains(strings.ToLower(s.Target), needle) ||
			strings.Contains(strings.ToLower(s.Group), needle) {
			out = append(out, s)
		}
	}
	return out
}

// selIndex resolves the selected endpoint name to a position in the current
// (possibly re-sorted, possibly filtered) list.
func (a *app) selIndex(list []stats.Summary) int {
	for i, s := range list {
		if s.Target == a.selName {
			return i
		}
	}
	return 0
}

func (a *app) moveSel(delta int) {
	list := a.visible()
	if len(list) == 0 {
		return
	}
	i := a.selIndex(list) + delta
	if i < 0 {
		i = 0
	}
	if i >= len(list) {
		i = len(list) - 1
	}
	a.selName = list[i].Target
}

func (a *app) handleKey(k key) (quit bool) {
	a.notice = ""

	// Help is a modal overlay: any key dismisses it, except the one that
	// quits outright.
	if a.view == viewHelp {
		if k.kind == keyCtrlC || (k.kind == keyRune && k.r == 'q') {
			return true
		}
		a.view = a.prevView
		return false
	}

	// Filter entry swallows most keys while active.
	if a.filtering {
		switch k.kind {
		case keyEnter, keyEsc:
			a.filtering = false
			if k.kind == keyEsc {
				a.filter = ""
			}
		case keyBackspace:
			if n := len(a.filter); n > 0 {
				a.filter = a.filter[:n-1]
			}
		case keyRune:
			a.filter += string(k.r)
		case keyCtrlC:
			return true
		}
		return false
	}

	switch k.kind {
	case keyCtrlC:
		return true
	case keyUp:
		a.moveSel(-1)
		return false
	case keyDown:
		a.moveSel(1)
		return false
	case keyPgUp:
		a.moveSel(-10)
		return false
	case keyPgDn:
		a.moveSel(10)
		return false
	case keyHome:
		if list := a.visible(); len(list) > 0 {
			a.selName = list[0].Target
		}
		return false
	case keyEnd:
		if list := a.visible(); len(list) > 0 {
			a.selName = list[len(list)-1].Target
		}
		return false
	case keyEnter, keyRight:
		if a.view == viewList {
			a.view = viewDetail
		}
		return false
	case keyLeft, keyEsc:
		if a.view != viewList {
			a.view = viewList
		}
		return false
	case keyTab:
		if a.view == viewDetail {
			a.view = viewList
		} else {
			a.view = viewDetail
		}
		return false
	}

	switch k.r {
	case 'q':
		return true
	case 'j':
		a.moveSel(1)
	case 'k':
		a.moveSel(-1)
	case 'g':
		if list := a.visible(); len(list) > 0 {
			a.selName = list[0].Target
		}
	case 'G':
		if list := a.visible(); len(list) > 0 {
			a.selName = list[len(list)-1].Target
		}
	case 'l':
		a.view = viewDetail
	case 'h':
		a.view = viewList
	case 's':
		a.sortIdx = (a.sortIdx + 1) % len(sortKeys)
		stats.SortSummaries(a.sums, sortKeys[a.sortIdx])
		a.notice = "sort: " + sortKeys[a.sortIdx]
	case 'S':
		a.sortIdx = (a.sortIdx - 1 + len(sortKeys)) % len(sortKeys)
		stats.SortSummaries(a.sums, sortKeys[a.sortIdx])
		a.notice = "sort: " + sortKeys[a.sortIdx]
	case 'p':
		a.paused = !a.paused
		if a.paused {
			a.notice = "paused"
		}
	case 'r':
		a.reload()
	case '/':
		a.filtering = true
		a.filter = ""
	case '?':
		a.prevView = a.view
		a.view = viewHelp
	case '1', '2', '3', '4', '5':
		idx := int(k.r - '1')
		if idx < len(windows) {
			a.windowIdx = idx
			a.reload()
		}
	}
	return false
}

// reload asks the caller to refill the sample set for the current window.
func (a *app) reload() {
	to := time.Now()
	from := to.Add(-a.window())
	if a.opts.Reload != nil {
		if err := a.opts.Reload(from, to); err != nil {
			a.notice = "reload failed: " + err.Error()
		} else {
			a.notice = "reloaded " + windows[a.windowIdx].label
		}
	}
	a.recompute()
}

// ---------------------------------------------------------------------------
// drawing
// ---------------------------------------------------------------------------

func (a *app) draw() error {
	if a.scr.refreshSize() {
		// Match the chart resolution to the window so a wide terminal gets
		// more detail rather than a stretched picture.
		a.buckets = clamp(a.scr.width-24, 20, 240)
		a.recompute()
	}
	w, h := a.scr.width, a.scr.height

	var lines []string
	switch a.view {
	case viewHelp:
		lines = a.drawHelp(w, h)
	case viewDetail:
		lines = a.drawDetail(w, h)
	default:
		lines = a.drawList(w, h)
	}
	return a.scr.render(lines)
}

func (a *app) header(w int) []string {
	t := a.th
	o := a.overall

	left := t.bold("uptime-mon")
	if a.opts.Title != "" {
		left += t.dim("  " + a.opts.Title)
	}

	upNow, downNow := 0, 0
	for _, s := range a.sums {
		if s.Samples == 0 {
			continue
		}
		if s.LastStatus == model.StatusUp {
			upNow++
		} else {
			downNow++
		}
	}

	state := t.green(fmt.Sprintf("%d up", upNow))
	if downNow > 0 {
		state += "  " + t.red(fmt.Sprintf("%d DOWN", downNow))
	}
	right := fmt.Sprintf("%s  %s  %s",
		state,
		t.dim("window "+windows[a.windowIdx].label),
		t.dim(a.lastCalc.Format("15:04:05")))

	pad := w - width(left) - width(right)
	if pad < 1 {
		pad = 1
	}
	line1 := left + strings.Repeat(" ", pad) + right

	// Fleet-level summary line: the "at a glance" row.
	line2 := fmt.Sprintf(
		"%s %s   %s %s   %s %s   %s %s   %s %s   %s %s",
		t.dim("avail"), t.health(o.Availability, pct(o.Availability)),
		t.dim("p50"), ms(o.RTT.Median)+"ms",
		t.dim("p95"), ms(o.RTT.P95)+"ms",
		t.dim("p99"), ms(o.RTT.P99)+"ms",
		t.dim("apdex"), fmt.Sprintf("%.3f", o.Apdex),
		t.dim("samples"), compactInt(o.Samples),
	)
	return []string{line1, line2, t.grey(strings.Repeat("─", w))}
}

func (a *app) footer(w int, hints string) string {
	t := a.th
	if a.filtering {
		return t.bold("/") + a.filter + t.s(sgrRev, " ")
	}
	left := t.dim(hints)
	var right string
	switch {
	case a.notice != "":
		right = t.yellow(a.notice)
	case a.paused:
		right = t.yellow("paused")
	case a.filter != "":
		right = t.cyan("filter: " + a.filter)
	case a.opts.Status != nil:
		right = t.dim(a.opts.Status())
	}
	pad := w - width(left) - width(right)
	if pad < 1 {
		return trunc(left, w)
	}
	return left + strings.Repeat(" ", pad) + right
}

func (a *app) drawList(w, h int) []string {
	t := a.th
	lines := a.header(w)

	list := a.visible()
	if a.selName == "" && len(list) > 0 {
		a.selName = list[0].Target
	}

	// Column layout: give the sparkline whatever is left after the numbers.
	const (
		wState = 5
		wAvail = 8
		wP50   = 7
		wP95   = 7
		wLast  = 7
	)
	fixed := wState + wAvail + wP50 + wP95 + wLast + 6
	wName := clamp(w*40/100, 18, 60)
	wSpark := w - fixed - wName
	if wSpark < 8 {
		wName = clamp(w-fixed-8, 10, 60)
		wSpark = w - fixed - wName
	}
	if wSpark < 0 {
		wSpark = 0
	}

	head := padRight("ENDPOINT", wName) + " " +
		padLeft("ST", wState) + " " +
		padLeft("AVAIL", wAvail) + " " +
		padLeft("P50", wP50) + " " +
		padLeft("P95", wP95) + " " +
		padLeft("LAST", wLast) + " " +
		padRight(windows[a.windowIdx].label, wSpark)
	lines = append(lines, t.dim(head))

	// Reserve room for header, column head and footer.
	rows := h - len(lines) - 1
	if rows < 1 {
		rows = 1
	}
	sel := a.selIndex(list)
	a.scroll = clampScroll(a.scroll, sel, rows, len(list))

	for i := a.scroll; i < len(list) && i < a.scroll+rows; i++ {
		s := list[i]
		lines = append(lines, a.listRow(s, i == sel, wName, wState, wAvail, wP50, wP95, wLast, wSpark))
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}

	hints := "j/k move  ⏎ detail  1-5 window  s sort  / filter  p pause  r reload  ? help  q quit"
	if len(list) != len(a.sums) {
		hints = fmt.Sprintf("%d/%d shown  ", len(list), len(a.sums)) + hints
	}
	lines = append(lines, a.footer(w, hints))
	return lines
}

func (a *app) listRow(s stats.Summary, selected bool, wName, wState, wAvail, wP50, wP95, wLast, wSpark int) string {
	t := a.th

	name := s.Target
	if s.Group != "" {
		name = s.Group + "/" + name
	}

	state := t.grey("  ? ")
	switch {
	case s.Samples == 0:
	case s.LastStatus == model.StatusUp:
		state = t.green("  UP")
	default:
		state = t.red("DOWN")
	}

	avail := t.health(s.Availability, pct(s.Availability))
	if s.Samples == 0 {
		avail = t.grey("-")
	}

	last := "-"
	if s.Samples > 0 {
		if s.LastStatus == model.StatusUp {
			last = ms(lastRTT(s))
		} else {
			last = t.red("fail")
		}
	}

	// The sparkline shows p95 latency shape; the colour of each cell would be
	// redundant with the availability column, so latency gets the shape and
	// availability gets the strip underneath in the detail view.
	spark := ""
	if wSpark > 0 {
		vals := bucketValues(s.Buckets, wSpark, func(b stats.Bucket) float64 {
			if b.Up == 0 {
				return math.NaN()
			}
			return b.P95RTT
		})
		lo, hi := rangeOf(vals)
		spark = sparkline(t, vals, lo, hi)
		// Overlay failures: any bucket with a failure is worth seeing even if
		// its latency looked fine.
		avails := bucketValues(s.Buckets, wSpark, func(b stats.Bucket) float64 { return b.Availability() })
		spark = overlayFailures(t, spark, avails)
	}

	row := padRight(name, wName) + " " +
		padLeft(state, wState) + " " +
		padLeft(avail, wAvail) + " " +
		padLeft(ms(s.RTT.Median), wP50) + " " +
		padLeft(ms(s.RTT.P95), wP95) + " " +
		padLeft(last, wLast) + " " +
		spark

	if selected {
		return t.s(sgrRev, padRight(stripSGR(row), a.scr.width))
	}
	return row
}

func (a *app) drawDetail(w, h int) []string {
	t := a.th
	lines := a.header(w)

	list := a.visible()
	if len(list) == 0 {
		lines = append(lines, "", t.dim("  no endpoints"))
		for len(lines) < h-1 {
			lines = append(lines, "")
		}
		return append(lines, a.footer(w, "esc back  q quit"))
	}
	s := list[a.selIndex(list)]

	title := t.bold(s.Target)
	meta := []string{}
	if s.Group != "" {
		meta = append(meta, "group="+s.Group)
	}
	if s.Host != "" {
		meta = append(meta, "host="+s.Host)
	}
	if s.Kind != "" {
		meta = append(meta, "kind="+string(s.Kind))
	}
	lines = append(lines, trunc(title+"  "+t.dim(strings.Join(meta, "  ")), w))

	// Current state line.
	statusTxt := t.grey("no samples in window")
	if s.Samples > 0 {
		streak := "for " + shortDur(s.StreakLength)
		if s.StreakLength <= 0 {
			streak = "as of the last probe"
		}
		if s.LastStatus == model.StatusUp {
			statusTxt = t.green("UP " + streak)
		} else {
			statusTxt = t.red("DOWN " + streak)
		}
		detail := fmt.Sprintf("last %s at %s", ms(lastRTT(s))+"ms", s.LastSeen.Format("15:04:05"))
		if s.LastCode != 0 {
			detail = fmt.Sprintf("HTTP %d  ", s.LastCode) + detail
		}
		if s.LastErr != "" {
			detail += "  " + t.red(trunc(s.LastErr, 60))
		}
		statusTxt += "   " + t.dim(detail)
	}
	lines = append(lines, trunc(statusTxt, w), "")

	// Two stat columns.
	half := w/2 - 2
	if half < 24 {
		half = w - 2
	}
	left := a.availabilityBlock(s)
	right := a.latencyBlock(s)
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if half >= w-2 {
			lines = append(lines, "  "+l)
			continue
		}
		lines = append(lines, "  "+padRight(l, half)+"  "+r)
	}
	lines = append(lines, "")

	// Charts sized to whatever vertical room is left.
	remaining := h - len(lines) - 1
	chartH := clamp(remaining-6, 3, 10)
	chartW := w - 12
	if chartW > 8 && chartH >= 3 {
		lines = append(lines, a.rttChart(s, chartW, chartH)...)
		lines = append(lines, "")
		lines = append(lines, a.uptimeRow(s, w)...)
	}

	// Failure breakdown and recent outages, if there is room.
	if h-len(lines)-1 >= 3 {
		lines = append(lines, "")
		lines = append(lines, a.failureBlock(s, w, h-len(lines)-1)...)
	}

	// Charts and the failure block are sized from the space left over, but
	// rounding can still overshoot; trim so the footer is never the line that
	// falls off the bottom.
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	return append(lines, a.footer(w, "j/k prev/next  esc back  1-5 window  p pause  q quit"))
}

func (a *app) availabilityBlock(s stats.Summary) []string {
	t := a.th
	kv := func(k, v string) string { return t.dim(padRight(k, 11)) + padLeft(v, 12) }

	ninesTxt := "-"
	if !math.IsInf(s.Nines, 1) && s.Samples > 0 {
		ninesTxt = fmt.Sprintf("%.2f nines", s.Nines)
	} else if s.Samples > 0 {
		ninesTxt = "no failures"
	}

	out := []string{
		t.bold("AVAILABILITY"),
		kv("avail", pct(s.Availability)) + "  " + t.dim(ninesTxt),
		kv("samples", compactInt(s.Samples)),
		kv("up / down", fmt.Sprintf("%s / %s", compactInt(s.Up), compactInt(s.Down))),
		kv("apdex", fmt.Sprintf("%.3f", s.Apdex)) + "  " + t.dim("T="+shortDur(s.ApdexT)),
		kv("outages", fmt.Sprintf("%d", s.OutageCount)),
		kv("downtime", shortDur(s.Downtime)),
		kv("longest", shortDur(s.LongestOutage)),
		kv("MTTR", shortDur(s.MTTR)),
		kv("MTBF", shortDur(s.MTBF)),
		kv("interval", shortDur(s.Interval)),
	}
	if s.Gaps > 0 {
		out = append(out, kv("gaps", fmt.Sprintf("%d", s.Gaps))+"  "+t.yellow(shortDur(s.GapDuration)+" unmonitored"))
	}
	return out
}

func (a *app) latencyBlock(s stats.Summary) []string {
	t := a.th
	d := s.RTT
	kv := func(k string, v float64) string {
		return t.dim(padRight(k, 8)) + padLeft(ms(v), 9)
	}
	two := func(k1 string, v1 float64, k2 string, v2 float64) string {
		return kv(k1, v1) + "   " + kv(k2, v2)
	}
	// CV and skew are ratios, not durations, so they get plain decimals
	// rather than the millisecond formatter.
	ratio := func(k string, v float64) string {
		return t.dim(padRight(k, 8)) + padLeft(fmt.Sprintf("%.2f", v), 9)
	}
	out := []string{
		t.bold("LATENCY") + t.dim("  ms, successful probes only"),
		two("min", d.Min, "p90", d.P90),
		two("p25", d.P25, "p95", d.P95),
		two("median", d.Median, "p99", d.P99),
		two("mean", d.Mean, "max", d.Max),
		two("stddev", d.StdDev, "IQR", d.IQR),
		kv("MAD", d.MAD) + "   " + ratio("CV", d.CV),
		ratio("skew", d.Skew),
	}
	if s.FailRTT.N > 0 {
		out = append(out,
			"",
			t.bold("TIME TO FAIL")+t.dim(fmt.Sprintf("  n=%d", s.FailRTT.N)),
			two("median", s.FailRTT.Median, "max", s.FailRTT.Max),
		)
	}
	return out
}

func (a *app) rttChart(s stats.Summary, w, h int) []string {
	t := a.th
	vals := bucketValues(s.Buckets, w*2, func(b stats.Bucket) float64 {
		if b.Up == 0 {
			return math.NaN()
		}
		return b.MeanRTT
	})
	lo, hi := rangeOf(vals)
	rows := brailleChart(vals, w, h, lo, hi)

	out := make([]string, 0, len(rows)+1)
	title := t.bold("RTT") + t.dim(fmt.Sprintf("  mean per bucket, %s", windows[a.windowIdx].label))
	out = append(out, "  "+title)
	for i, r := range rows {
		var axis string
		switch i {
		case 0:
			axis = padLeft(ms(hi), 8)
		case len(rows) - 1:
			axis = padLeft(ms(lo), 8)
		default:
			axis = strings.Repeat(" ", 8)
		}
		out = append(out, t.dim(axis)+" "+t.cyan(r))
	}
	return out
}

// uptimeRow draws the availability strip. total is the full terminal width;
// the strip is sized so the finished row, gutter and "now" label together stay
// strictly inside it.
func (a *app) uptimeRow(s stats.Summary, total int) []string {
	t := a.th
	const gutter = 8 // width of the "-24h" label
	cells := total - gutter - len(" ") - len(" now") - 1
	if cells < 4 {
		return nil
	}
	avails := bucketValues(s.Buckets, cells, func(b stats.Bucket) float64 { return b.Availability() })
	span := windows[a.windowIdx].label
	return []string{
		"  " + t.bold("UPTIME") + t.dim("  each cell = "+shortDur(a.window()/time.Duration(max(1, len(avails))))),
		t.dim(padLeft("-"+span, gutter)) + " " + uptimeStrip(t, avails) + " " + t.dim("now"),
	}
}

func (a *app) failureBlock(s stats.Summary, w, maxRows int) []string {
	t := a.th
	if maxRows < 2 {
		return nil
	}
	half := w/2 - 2

	var lcol, rcol []string
	lcol = append(lcol, t.bold("FAILURES"))
	if len(s.Errors) == 0 {
		lcol = append(lcol, t.dim("  none"))
	}
	for i, e := range s.Errors {
		if i >= maxRows-1 {
			lcol = append(lcol, t.dim(fmt.Sprintf("  +%d more", len(s.Errors)-i)))
			break
		}
		lcol = append(lcol, fmt.Sprintf("  %s %s", padLeft(compactInt(e.Count), 5), trunc(e.Err, half-8)))
	}

	rcol = append(rcol, t.bold("RECENT OUTAGES"))
	if len(s.Outages) == 0 {
		rcol = append(rcol, t.dim("  none"))
	}
	// Newest first: an operator cares about the last one, not the first.
	for i := len(s.Outages) - 1; i >= 0; i-- {
		if len(rcol) >= maxRows {
			break
		}
		o := s.Outages[i]
		mark := "  "
		if o.Ongoing {
			mark = t.red(" •")
		}
		rcol = append(rcol, fmt.Sprintf("%s %s  %s  %s",
			mark, o.Start.Format("01-02 15:04"),
			padLeft(shortDur(o.Duration()), 7),
			trunc(o.Reason, half-30)))
	}

	n := len(lcol)
	if len(rcol) > n {
		n = len(rcol)
	}
	if n > maxRows {
		n = maxRows
	}
	out := make([]string, 0, n)
	for i := range n {
		var l, r string
		if i < len(lcol) {
			l = lcol[i]
		}
		if i < len(rcol) {
			r = rcol[i]
		}
		out = append(out, "  "+padRight(l, half)+"  "+r)
	}
	return out
}

func (a *app) drawHelp(w, h int) []string {
	t := a.th
	lines := []string{t.bold("  uptime-mon — keys"), ""}
	rows := [][2]string{
		{"j / k / ↑ / ↓", "move selection"},
		{"g / G / Home / End", "first / last endpoint"},
		{"PgUp / PgDn", "move by ten"},
		{"Enter / l / →", "endpoint detail"},
		{"Esc / h / ←", "back to the list"},
		{"Tab", "toggle list and detail"},
		{"1 2 3 4 5", "window: 1h, 6h, 24h, 7d, 30d"},
		{"s / S", "cycle sort forward / back"},
		{"/", "filter by name or group (Enter to apply, Esc to clear)"},
		{"p", "pause recomputation"},
		{"r", "reload the window from disk"},
		{"?", "this help"},
		{"q / Ctrl-C", "quit"},
	}
	for _, r := range rows {
		lines = append(lines, "  "+t.cyan(padRight(r[0], 20))+t.dim(r[1]))
	}
	lines = append(lines,
		"",
		"  "+t.bold("reading the numbers"),
		"  "+t.dim("avail    fraction of probes that succeeded, not time-weighted"),
		"  "+t.dim("p50/p95  latency percentiles over successful probes only"),
		"  "+t.dim("MAD      median absolute deviation: jitter, robust to outliers"),
		"  "+t.dim("CV       stddev/mean: relative spread, comparable across endpoints"),
		"  "+t.dim("apdex    1.0 all fast, 0.5 all tolerable, 0 all slow or failed"),
		"  "+t.dim("MTTR     mean outage length; MTBF mean uptime between outages"),
		"  "+t.dim("gaps     stretches where the monitor itself was not running"),
	)
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	return append(lines, a.footer(w, "any key to return"))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// bucketValues resamples the summary's buckets to exactly n points.
func bucketValues(bs []stats.Bucket, n int, get func(stats.Bucket) float64) []float64 {
	if n <= 0 {
		return nil
	}
	out := make([]float64, n)
	if len(bs) == 0 {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	for i := range n {
		// Average the source buckets that fall into this output slot, so
		// downsampling does not drop a spike entirely.
		lo := i * len(bs) / n
		hi := (i + 1) * len(bs) / n
		if hi <= lo {
			hi = lo + 1
		}
		if hi > len(bs) {
			hi = len(bs)
		}
		sum, cnt := 0.0, 0
		for j := lo; j < hi; j++ {
			v := get(bs[j])
			if math.IsNaN(v) {
				continue
			}
			sum += v
			cnt++
		}
		if cnt == 0 {
			out[i] = math.NaN()
		} else {
			out[i] = sum / float64(cnt)
		}
	}
	return out
}

func rangeOf(vals []float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if math.IsNaN(v) {
			continue
		}
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	if math.IsInf(lo, 1) {
		return 0, 1
	}
	if hi <= lo {
		hi = lo + 1
	}
	return lo, hi
}

// overlayFailures replaces sparkline cells whose bucket had failures with a
// red marker, so a fast-but-failing endpoint cannot look healthy.
func overlayFailures(t theme, spark string, avails []float64) string {
	if spark == "" {
		return spark
	}
	cells := splitCells(spark)
	for i := 0; i < len(cells) && i < len(avails); i++ {
		a := avails[i]
		if math.IsNaN(a) || a >= 1 {
			continue
		}
		if a == 0 {
			cells[i] = t.red("▁")
		} else {
			cells[i] = t.yellow(stripSGR(cells[i]))
		}
	}
	return strings.Join(cells, "")
}

// splitCells breaks a rendered string into per-column pieces, keeping each
// character's SGR sequences attached to it.
//
// The rule that makes this work: a reset sequence closes the character it
// follows, while any other sequence opens the character it precedes. So
// "\x1b[31ma\x1b[0mb" splits into a red "a" and a plain "b" rather than
// smearing the reset onto the wrong cell.
func splitCells(s string) []string {
	var (
		out      []string
		cur      strings.Builder
		pending  strings.Builder
		esc      strings.Builder
		haveChar bool
		inEsc    bool
	)
	flush := func() {
		if haveChar {
			out = append(out, cur.String())
			cur.Reset()
			haveChar = false
		}
	}

	for _, r := range s {
		if inEsc {
			esc.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
				seq := esc.String()
				esc.Reset()
				if seq == sgrReset && haveChar {
					cur.WriteString(seq)
					flush()
				} else {
					pending.WriteString(seq)
				}
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			esc.Reset()
			esc.WriteRune(r)
			continue
		}

		flush()
		if pending.Len() > 0 {
			cur.WriteString(pending.String())
			pending.Reset()
		}
		cur.WriteRune(r)
		haveChar = true
	}
	flush()

	// Trailing styles with no character of their own belong to the last cell.
	if tail := cur.String() + pending.String(); tail != "" {
		if len(out) > 0 {
			out[len(out)-1] += tail
		} else {
			out = append(out, tail)
		}
	}
	return out
}

func stripSGR(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// lastRTT is the latency of the most recent probe, in milliseconds.
func lastRTT(s stats.Summary) float64 {
	return float64(s.LastRTT) / float64(time.Millisecond)
}

func compactInt(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampScroll(scroll, sel, rows, n int) int {
	if n <= rows {
		return 0
	}
	if sel < scroll {
		scroll = sel
	}
	if sel >= scroll+rows {
		scroll = sel - rows + 1
	}
	if scroll > n-rows {
		scroll = n - rows
	}
	if scroll < 0 {
		scroll = 0
	}
	return scroll
}

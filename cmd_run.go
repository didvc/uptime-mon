package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/didvc/uptime-mon/internal/collect"
	"github.com/didvc/uptime-mon/internal/endpoints"
	"github.com/didvc/uptime-mon/internal/lineproto"
	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/probe"
	"github.com/didvc/uptime-mon/internal/series"
	"github.com/didvc/uptime-mon/internal/store"
	"github.com/didvc/uptime-mon/internal/tui"
)

// runConfig is the flag set shared by `run` and `check`.
type runConfig struct {
	endpointsPath string
	dataDir       string
	prefix        string

	// target overrides
	interval time.Duration
	timeout  time.Duration
	retries  int
	insecure bool
	ua       string
	group    string
	endpoint string

	// probe behaviour
	concurrency  int
	jitter       float64
	noKeepAlive  bool
	noHTTP2      bool
	resolver     string
	pingFallback int

	// storage
	flushEvery       time.Duration
	flushBytes       int
	fsync            bool
	compress         bool
	compressLevel    int
	keepUncompressed bool
	retentionDays    int
	utc              bool
	measurement      string
	precision        string
	tags             kvFlag

	// presentation
	withTUI    bool
	window     time.Duration
	refresh    time.Duration
	apdexT     time.Duration
	maxSamples int
	noColor    bool
	verbose    bool
	quiet      bool
	once       bool
}

func (c *runConfig) bindCommon(fs *flag.FlagSet) {
	fs.StringVar(&c.endpointsPath, "endpoints", "endpoints.txt", "endpoints file to read")
	fs.StringVar(&c.group, "group", "", "only targets in this group")
	fs.StringVar(&c.endpoint, "endpoint", "", "only targets whose name contains this substring")

	fs.DurationVar(&c.interval, "interval", 0, "override the probe interval for every target")
	fs.DurationVar(&c.timeout, "timeout", 0, "override the probe timeout for every target")
	fs.IntVar(&c.retries, "retries", -1, "override the retry count for every target")
	fs.BoolVar(&c.insecure, "insecure", false, "skip TLS certificate verification everywhere")
	fs.StringVar(&c.ua, "ua", "", "override the User-Agent header")

	fs.IntVar(&c.concurrency, "concurrency", 8, "maximum probes in flight at once")
	fs.BoolVar(&c.noKeepAlive, "no-keepalive", false, "open a fresh connection for every probe")
	fs.BoolVar(&c.noHTTP2, "no-http2", false, "force HTTP/1.1")
	fs.StringVar(&c.resolver, "resolver", "", "DNS server to use, e.g. 1.1.1.1:53")
	fs.IntVar(&c.pingFallback, "ping-fallback-port", 443,
		"TCP port to probe when ICMP is unavailable; 0 to fail instead")
	fs.BoolVar(&c.noColor, "no-color", false, "disable ANSI colour")
}

func (c *runConfig) bindStorage(fs *flag.FlagSet) {
	fs.StringVar(&c.dataDir, "data", "./data", "directory for the daily line-protocol files")
	fs.StringVar(&c.prefix, "prefix", "uptime", "filename prefix for day files")
	fs.DurationVar(&c.flushEvery, "flush", 10*time.Minute,
		"batching window; samples are held in memory until it elapses")
	fs.IntVar(&c.flushBytes, "flush-bytes", 4<<20, "force a flush once the buffer exceeds this many bytes")
	fs.BoolVar(&c.fsync, "fsync", false, "fsync after every flush")
	fs.BoolVar(&c.compress, "compress", true, "zstd-compress each day once it is complete")
	fs.IntVar(&c.compressLevel, "compress-level", 3, "zstd level, 1 (fastest) to 19 (smallest)")
	fs.BoolVar(&c.keepUncompressed, "keep-uncompressed", false, "keep the .lp file alongside the .lp.zst")
	fs.IntVar(&c.retentionDays, "retention-days", 0, "delete day files older than this; 0 keeps everything")
	fs.BoolVar(&c.utc, "utc", false, "place day boundaries at UTC midnight instead of local")
	fs.StringVar(&c.measurement, "measurement", "uptime", "line-protocol measurement name")
	fs.StringVar(&c.precision, "precision", "ns", "timestamp precision: ns, us, ms or s")
	fs.Var(&c.tags, "tag", "extra static tag as key=value; repeatable")
}

// load reads endpoints.txt and applies the command-line overrides.
func (c *runConfig) load() ([]model.Target, error) {
	def := endpoints.BaseDefaults()
	if c.interval > 0 {
		def.Interval = c.interval
	}
	if c.timeout > 0 {
		def.Timeout = c.timeout
	}
	if c.retries >= 0 {
		def.Retries = c.retries
	}
	if c.insecure {
		def.Insecure = true
	}
	if c.ua != "" {
		def.UserAgent = c.ua
	}

	targets, err := endpoints.LoadFile(c.endpointsPath, def)
	if err != nil {
		return nil, err
	}

	// Flag overrides beat the file, so that a one-off "-interval 5s" run does
	// what it says even though every line has an interval of its own.
	for i := range targets {
		if c.interval > 0 {
			targets[i].Interval = c.interval
		}
		if c.timeout > 0 {
			targets[i].Timeout = c.timeout
		}
		if c.retries >= 0 {
			targets[i].Retries = c.retries
		}
		if c.insecure {
			targets[i].Insecure = true
		}
		if c.ua != "" {
			targets[i].UserAgent = c.ua
		}
	}

	targets = endpoints.Filter(targets, c.group, c.endpoint, false)
	if len(targets) == 0 {
		return nil, fmt.Errorf("no targets matched the filters")
	}
	return targets, nil
}

func (c *runConfig) proberOptions() probe.Options {
	po := probe.DefaultOptions()
	po.KeepAlive = !c.noKeepAlive
	po.DisableHTTP2 = c.noHTTP2
	po.Resolver = c.resolver
	return po
}

func (c *runConfig) storeConfig() (store.Config, error) {
	prec, err := lineproto.ParsePrecision(c.precision)
	if err != nil {
		return store.Config{}, err
	}
	var extra []model.Header
	for _, kv := range c.tags {
		extra = append(extra, model.Header{Key: kv.K, Value: kv.V})
	}
	return store.Config{
		Dir:              c.dataDir,
		Prefix:           c.prefix,
		Measurement:      c.measurement,
		Precision:        prec,
		ExtraTags:        extra,
		FlushInterval:    c.flushEvery,
		FlushBytes:       c.flushBytes,
		Fsync:            c.fsync,
		Compress:         c.compress,
		CompressLevel:    c.compressLevel,
		KeepUncompressed: c.keepUncompressed,
		RetentionDays:    c.retentionDays,
		UTC:              c.utc,
	}, nil
}

// ---------------------------------------------------------------------------

func cmdRun(args []string) error {
	var c runConfig
	fs := newFlagSet("run", "Probe every enabled endpoint on its own schedule and append\n"+
		"the results to daily line-protocol files.\n\n"+
		"Samples are batched in memory and flushed every -flush interval, so an\n"+
		"unclean kill loses at most that much data. Lower it, or set -fsync, if\n"+
		"that matters more than write volume.")
	c.bindCommon(fs)
	c.bindStorage(fs)
	fs.BoolVar(&c.withTUI, "tui", false, "show the live terminal UI instead of log lines")
	fs.BoolVar(&c.once, "once", false, "probe each target once, then exit")
	fs.BoolVar(&c.verbose, "v", false, "log every probe, not just state changes")
	fs.BoolVar(&c.quiet, "quiet", false, "log nothing but errors")
	fs.DurationVar(&c.window, "window", 24*time.Hour, "initial TUI window")
	fs.DurationVar(&c.refresh, "refresh", time.Second, "TUI redraw interval")
	fs.DurationVar(&c.apdexT, "apdex", 500*time.Millisecond, "Apdex target latency T")
	fs.IntVar(&c.maxSamples, "max-samples", 0,
		"in-memory samples kept per endpoint for the TUI; 0 sizes it from -window")
	if err := fs.Parse(args); err != nil {
		return err
	}

	targets, err := c.load()
	if err != nil {
		return err
	}

	if n, note := collect.ApplyPingFallback(targets, c.pingFallback); note != "" {
		fmt.Fprintf(os.Stderr, "uptime-mon: %s\n", note)
		_ = n
	}

	scfg, err := c.storeConfig()
	if err != nil {
		return err
	}
	w, err := store.Open(scfg)
	if err != nil {
		return err
	}
	w.OnError = func(e error) { fmt.Fprintf(os.Stderr, "uptime-mon: %v\n", e) }
	defer func() {
		if cerr := w.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "uptime-mon: closing store: %v\n", cerr)
		}
	}()

	p := probe.New(c.proberOptions())
	defer p.Close()

	set := series.NewSet(c.sampleCap(targets))

	// State-change logging. This is not alerting: nothing is delivered
	// anywhere, no thresholds are configured and no one is woken up. It is
	// just the subset of the log that a person reading a terminal wants.
	var (
		mu       sync.Mutex
		lastUp   = map[string]model.Status{}
		firstFor = map[string]bool{}
	)
	logResult := func(r model.Result) {
		if c.quiet {
			return
		}
		mu.Lock()
		prev, seen := lastUp[r.Target]
		lastUp[r.Target] = r.Status
		first := !firstFor[r.Target]
		firstFor[r.Target] = true
		mu.Unlock()

		changed := !seen || prev != r.Status
		if !c.verbose && !changed {
			return
		}
		if !c.verbose && first && r.Status == model.StatusUp {
			return // do not narrate 90 endpoints coming up at startup
		}
		fmt.Fprintln(os.Stderr, formatResult(r, colorEnabled(c.noColor, true)))
	}

	onResult := func(r model.Result) {
		w.Add(r)
		set.Add(r)
		if !c.withTUI {
			logResult(r)
		}
	}

	col := collect.New(targets, p, collect.Options{
		Concurrency: c.concurrency,
		Jitter:      0.2,
		OnResult:    onResult,
	})
	if len(col.Targets()) == 0 {
		return fmt.Errorf("every matching target is disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if c.once {
		results := col.Once(ctx)
		if !c.withTUI {
			printCheckTable(results, colorEnabled(c.noColor, true))
		}
		return nil
	}

	if !c.withTUI && !c.quiet {
		fmt.Fprintf(os.Stderr,
			"uptime-mon %s: watching %d endpoint(s), writing to %s, flushing every %s\n",
			version, len(col.Targets()), c.dataDir, c.flushEvery)
	}

	// Warm the TUI with what is already on disk, so it opens on history
	// rather than on an empty chart that fills in over the next hour.
	reload := makeReloader(set, scfg)
	if c.withTUI {
		if err := reload(time.Now().Add(-c.window), time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "uptime-mon: could not load history: %v\n", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		col.Run(ctx)
	}()

	if c.withTUI {
		err = tui.Run(ctx, tui.Options{
			Set:     set,
			Reload:  reload,
			Title:   c.dataDir,
			Window:  c.window,
			Refresh: c.refresh,
			ApdexT:  c.apdexT,
			Color:   colorEnabled(c.noColor, false),
			Status: func() string {
				done, inflight, _ := col.Stats()
				return fmt.Sprintf("%d probes  %d in flight  %s buffered",
					done, inflight, humanBytes(int64(w.Buffered())))
			},
		})
		stop()
	} else {
		<-ctx.Done()
	}
	wg.Wait()

	if !c.quiet && !c.withTUI {
		fmt.Fprintf(os.Stderr, "uptime-mon: stopping, flushing %s\n", humanBytes(int64(w.Buffered())))
	}
	return err
}

// sampleCap sizes the in-memory ring from the TUI window and the fastest
// target interval, so the live view has exactly the history it can display and
// not a byte more.
func (c *runConfig) sampleCap(targets []model.Target) int {
	if c.maxSamples > 0 {
		return c.maxSamples
	}
	shortest := time.Hour
	for _, t := range targets {
		if t.Enabled && t.Interval > 0 && t.Interval < shortest {
			shortest = t.Interval
		}
	}
	if shortest <= 0 {
		shortest = time.Minute
	}
	n := int(c.window/shortest) + 64
	// Clamp so a "-window 30d -interval 1s" typo cannot ask for a gigabyte.
	if n < 256 {
		n = 256
	}
	if n > 200000 {
		n = 200000
	}
	return n
}

// makeReloader returns a function that refills the sample set from disk.
func makeReloader(set *series.Set, scfg store.Config) func(from, to time.Time) error {
	r := &store.Reader{Dir: scfg.Dir, Prefix: scfg.Prefix, Precision: scfg.Precision}
	var mu sync.Mutex
	return func(from, to time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		set.Reset()
		return r.Scan(from, to, func(res model.Result) error {
			set.Add(res)
			return nil
		})
	}
}

// ---------------------------------------------------------------------------

func cmdCheck(args []string) error {
	var c runConfig
	fs := newFlagSet("check", "Probe every matching endpoint once and print the outcome.\n"+
		"Nothing is written to disk. Exits non-zero if any endpoint is down,\n"+
		"which makes it usable as a smoke test in a script.")
	c.bindCommon(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	targets, err := c.load()
	if err != nil {
		return err
	}
	if _, note := collect.ApplyPingFallback(targets, c.pingFallback); note != "" {
		fmt.Fprintf(os.Stderr, "uptime-mon: %s\n", note)
	}

	p := probe.New(c.proberOptions())
	defer p.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	col := collect.New(targets, p, collect.Options{Concurrency: c.concurrency})
	results := col.Once(ctx)
	printCheckTable(results, colorEnabled(c.noColor, true))

	down := 0
	for _, r := range results {
		if r.Status != model.StatusUp {
			down++
		}
	}
	if down > 0 {
		return fmt.Errorf("%d of %d endpoint(s) down", down, len(results))
	}
	return nil
}

func printCheckTable(results []model.Result, color bool) {
	sorted := append([]model.Result(nil), results...)
	sort.Slice(sorted, func(i, j int) bool {
		if (sorted[i].Status == model.StatusUp) != (sorted[j].Status == model.StatusUp) {
			return sorted[i].Status != model.StatusUp // failures first
		}
		return sorted[i].Target < sorted[j].Target
	})
	for _, r := range sorted {
		fmt.Println(formatResult(r, color))
	}
}

func formatResult(r model.Result, color bool) string {
	state := "UP  "
	if r.Status != model.StatusUp {
		state = "DOWN"
	}
	if color {
		if r.Status == model.StatusUp {
			state = "\x1b[32m" + state + "\x1b[0m"
		} else {
			state = "\x1b[31m" + state + "\x1b[0m"
		}
	}

	rtt := fmt.Sprintf("%7.1fms", float64(r.RTT)/float64(time.Millisecond))
	code := "    "
	if r.Code != 0 {
		code = fmt.Sprintf("%4d", r.Code)
	}
	line := fmt.Sprintf("%s %s %s %s %s",
		r.At.Format("15:04:05"), state, code, rtt, r.Target)
	if r.Attempt > 1 {
		line += fmt.Sprintf(" (attempt %d)", r.Attempt)
	}
	if r.Err != "" {
		line += "  " + r.Err
	}
	return line
}

func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1024*1024*1024))
	}
}

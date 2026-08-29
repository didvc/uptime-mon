// Package collect schedules probes and fans results out to the store and the
// live view.
//
// The scheduler is one goroutine per target holding a ticker. That sounds
// extravagant until you price it: a goroutine is a few kilobytes of stack, so
// ninety endpoints cost well under a megabyte, and in exchange each endpoint
// keeps its own independent period with no central wheel to get behind. A
// shared semaphore bounds how many probes are actually in flight, so a
// hundred endpoints coming due in the same second do not open a hundred
// sockets at once.
package collect

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/probe"
)

// Options tunes the scheduler.
type Options struct {
	// Concurrency caps simultaneous in-flight probes. Deliberately small by
	// default: monitoring should be invisible to the things it monitors, and
	// a burst of parallel requests measures your own contention as much as
	// the endpoint's health.
	Concurrency int

	// Jitter spreads the first probe of each target across this fraction of
	// its interval, so targets sharing a period do not stay in lockstep.
	Jitter float64

	// OnResult receives every result, in the order each target produces them.
	// Called from the probe goroutine, so implementations must be quick and
	// safe for concurrent use.
	OnResult func(model.Result)

	// OnError reports scheduler-level problems.
	OnError func(error)
}

// DefaultOptions returns the scheduler defaults.
func DefaultOptions() Options {
	return Options{Concurrency: 8, Jitter: 0.2}
}

// Collector runs the probe schedule.
type Collector struct {
	targets []model.Target
	prober  *probe.Prober
	opts    Options

	sem chan struct{}

	completed atomic.Uint64
	inflight  atomic.Int64
	started   time.Time
}

// New builds a Collector over the enabled subset of targets.
func New(targets []model.Target, p *probe.Prober, opts Options) *Collector {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.Jitter < 0 {
		opts.Jitter = 0
	}
	if opts.Jitter > 1 {
		opts.Jitter = 1
	}
	var enabled []model.Target
	for _, t := range targets {
		if t.Enabled {
			enabled = append(enabled, t)
		}
	}
	return &Collector{
		targets: enabled,
		prober:  p,
		opts:    opts,
		sem:     make(chan struct{}, opts.Concurrency),
	}
}

// Targets returns the enabled targets this collector will probe.
func (c *Collector) Targets() []model.Target { return c.targets }

// Stats reports progress for the status line.
func (c *Collector) Stats() (completed uint64, inflight int64, since time.Time) {
	return c.completed.Load(), c.inflight.Load(), c.started
}

// Run probes on schedule until ctx is cancelled, then waits for in-flight
// probes to finish.
func (c *Collector) Run(ctx context.Context) {
	c.started = time.Now()

	var wg sync.WaitGroup
	for i, t := range c.targets {
		wg.Add(1)
		go func(idx int, tg model.Target) {
			defer wg.Done()
			c.runTarget(ctx, idx, tg)
		}(i, t)
	}
	wg.Wait()
}

func (c *Collector) runTarget(ctx context.Context, idx int, t model.Target) {
	interval := t.Interval
	if interval <= 0 {
		interval = time.Minute
	}

	// Stagger the first probe. The index term guarantees a spread even if the
	// random draws clump; the random term stops the spread from being an
	// exactly repeating pattern across restarts.
	spread := time.Duration(float64(interval) * c.opts.Jitter)
	var delay time.Duration
	if spread > 0 && len(c.targets) > 0 {
		base := time.Duration(int64(spread) * int64(idx) / int64(len(c.targets)))
		delay = base + time.Duration(rand.Int63n(int64(spread/time.Duration(len(c.targets)))+1)) //nolint:gosec // scheduling, not security
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	c.probeOnce(ctx, t)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A Ticker drops ticks rather than queueing them, so a probe that
			// overruns its interval simply misses a beat instead of building
			// a backlog that can never be worked off.
			c.probeOnce(ctx, t)
		}
	}
}

func (c *Collector) probeOnce(ctx context.Context, t model.Target) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-c.sem }()

	c.inflight.Add(1)
	defer c.inflight.Add(-1)

	r, ok := c.prober.Probe(ctx, t)
	if !ok {
		return
	}
	c.completed.Add(1)
	if c.opts.OnResult != nil {
		c.opts.OnResult(r)
	}
}

// Once probes every target a single time and returns the results in target
// order. Used by `check` and by `run -once`.
func (c *Collector) Once(ctx context.Context) []model.Result {
	c.started = time.Now()
	out := make([]model.Result, len(c.targets))
	found := make([]bool, len(c.targets))

	var wg sync.WaitGroup
	for i, t := range c.targets {
		wg.Add(1)
		go func(idx int, tg model.Target) {
			defer wg.Done()
			select {
			case c.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-c.sem }()

			c.inflight.Add(1)
			defer c.inflight.Add(-1)

			r, ok := c.prober.Probe(ctx, tg)
			if !ok {
				return
			}
			c.completed.Add(1)
			out[idx], found[idx] = r, true
			if c.opts.OnResult != nil {
				c.opts.OnResult(r)
			}
		}(i, t)
	}
	wg.Wait()

	res := out[:0]
	for i, ok := range found {
		if ok {
			res = append(res, out[i])
		}
	}
	return res
}

// ApplyPingFallback rewrites ping targets into TCP-connect targets when the
// kernel will not let us send ICMP unprivileged.
//
// This happens once at startup rather than per probe, on purpose: a data file
// where the same endpoint silently switches measurement method partway through
// is worse than one that is honestly labelled tcp from the first sample. The
// returned note is meant to be printed, so the substitution is never a
// surprise.
func ApplyPingFallback(targets []model.Target, port int) (converted int, note string) {
	hasPing := false
	for _, t := range targets {
		if t.Enabled && t.Kind == model.KindPing {
			hasPing = true
			break
		}
	}
	if !hasPing {
		return 0, ""
	}
	err := probe.PingAvailable()
	if err == nil {
		return 0, ""
	}
	if port <= 0 {
		return 0, fmt.Sprintf("ICMP unavailable (%v); ping targets will fail. "+
			"Enable it, or set -ping-fallback-port, or rewrite them as tcp:// in endpoints.txt.", err)
	}
	for i := range targets {
		if targets[i].Kind != model.KindPing {
			continue
		}
		targets[i].Kind = model.KindTCP
		targets[i].Port = port
		converted++
	}
	return converted, fmt.Sprintf(
		"ICMP unavailable (%v);\n  %d ping target(s) rewritten as TCP connect on port %d "+
			"and recorded with kind=tcp.", err, converted, port)
}

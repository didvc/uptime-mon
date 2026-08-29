package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/probe"
)

func target(name, url string, interval time.Duration) model.Target {
	return model.Target{
		Name: name, Kind: model.KindHTTP, URL: url, Method: "GET",
		Accept: model.DefaultAccept(), Interval: interval, Timeout: 2 * time.Second,
		MaxRedirects: 10, MaxBody: 1 << 20, Enabled: true,
	}
}

func TestOnceProbesEveryEnabledTarget(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	p := probe.New(probe.DefaultOptions())
	defer p.Close()

	targets := []model.Target{
		target("a", srv.URL, time.Minute),
		target("b", srv.URL, time.Minute),
		target("disabled", srv.URL, time.Minute),
	}
	targets[2].Enabled = false

	var mu sync.Mutex
	var seen []model.Result
	c := New(targets, p, Options{
		Concurrency: 2,
		OnResult: func(r model.Result) {
			mu.Lock()
			seen = append(seen, r)
			mu.Unlock()
		},
	})

	if len(c.Targets()) != 2 {
		t.Fatalf("expected 2 enabled targets, got %d", len(c.Targets()))
	}

	got := c.Once(context.Background())
	if len(got) != 2 || len(seen) != 2 {
		t.Fatalf("got %d results, %d callbacks", len(got), len(seen))
	}
	if got[0].Target != "a" || got[1].Target != "b" {
		t.Errorf("results out of target order: %+v", got)
	}
	for _, r := range got {
		if r.Status != model.StatusUp {
			t.Errorf("%s: %+v", r.Target, r)
		}
	}
	if hits.Load() != 2 {
		t.Errorf("server saw %d requests", hits.Load())
	}
	if done, inflight, _ := c.Stats(); done != 2 || inflight != 0 {
		t.Errorf("stats = %d done, %d inflight", done, inflight)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	var cur, peak atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	p := probe.New(probe.DefaultOptions())
	defer p.Close()

	var targets []model.Target
	for i := range 12 {
		targets = append(targets, target(fmt.Sprintf("t%d", i), srv.URL, time.Minute))
	}
	c := New(targets, p, Options{Concurrency: 3})
	c.Once(context.Background())

	if peak.Load() > 3 {
		t.Errorf("peak concurrency %d exceeded the limit of 3", peak.Load())
	}
	if peak.Load() < 2 {
		t.Errorf("peak concurrency %d suggests probes did not overlap at all", peak.Load())
	}
}

func TestRunRepeatsUntilContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	p := probe.New(probe.DefaultOptions())
	defer p.Close()

	var count atomic.Int64
	c := New([]model.Target{target("a", srv.URL, 40*time.Millisecond)}, p, Options{
		Concurrency: 2,
		Jitter:      0, // deterministic: probe immediately, then every 40ms
		OnResult:    func(model.Result) { count.Add(1) },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 260*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	// Immediate probe plus roughly six ticks; assert a range, not a count, so
	// the test does not depend on scheduler precision.
	if n := count.Load(); n < 3 || n > 12 {
		t.Errorf("got %d probes in 260ms at a 40ms interval", n)
	}
}

func TestRunReturnsImmediatelyOnCancelledContext(t *testing.T) {
	p := probe.New(probe.DefaultOptions())
	defer p.Close()

	c := New([]model.Target{target("a", "http://127.0.0.1:1", time.Minute)}, p, DefaultOptions())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run blocked on a cancelled context")
	}
}

func TestApplyPingFallback(t *testing.T) {
	targets := []model.Target{
		{Name: "p", Kind: model.KindPing, Host: "example.com", Enabled: true},
		{Name: "h", Kind: model.KindHTTP, URL: "https://x/", Enabled: true},
	}
	n, note := ApplyPingFallback(targets, 443)

	if err := probe.PingAvailable(); err != nil {
		// ICMP unavailable: the ping target must have been rewritten, and the
		// substitution must be announced rather than silent.
		if n != 1 || note == "" {
			t.Fatalf("expected a rewrite with a note, got n=%d note=%q", n, note)
		}
		if targets[0].Kind != model.KindTCP || targets[0].Port != 443 {
			t.Errorf("ping target not converted: %+v", targets[0])
		}
		if targets[1].Kind != model.KindHTTP {
			t.Errorf("http target should be untouched: %+v", targets[1])
		}
	} else {
		if n != 0 || note != "" {
			t.Errorf("ICMP works here, so nothing should change: n=%d note=%q", n, note)
		}
	}

	// No ping targets at all means no capability check and no note.
	only := []model.Target{{Name: "h", Kind: model.KindHTTP, Enabled: true}}
	if n, note := ApplyPingFallback(only, 443); n != 0 || note != "" {
		t.Errorf("got n=%d note=%q", n, note)
	}
}

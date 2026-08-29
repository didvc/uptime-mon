package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/didvc/uptime-mon/internal/lineproto"
	"github.com/didvc/uptime-mon/internal/model"
	"github.com/didvc/uptime-mon/internal/series"
	"github.com/didvc/uptime-mon/internal/store"
	"github.com/didvc/uptime-mon/internal/tui"
)

// isTerminal reports whether stdout is interactive.
func isTerminal() bool { return tui.IsTerminal() }

func cmdTUI(args []string) error {
	fs := newFlagSet("tui", "Browse recorded data in the terminal UI, read-only.\n\n"+
		"This reads the day files; it does not probe anything. Use `run -tui`\n"+
		"to watch live results from a collector in the same process.")

	dir := fs.String("data", "./data", "data directory")
	prefix := fs.String("prefix", "uptime", "day-file prefix")
	precision := fs.String("precision", "ns", "timestamp precision the files were written with")
	window := fs.Duration("window", 24*time.Hour, "initial window")
	refresh := fs.Duration("refresh", 5*time.Second, "how often to re-read the files")
	apdexT := fs.Duration("apdex", 500*time.Millisecond, "Apdex target latency T")
	endpoint := fs.String("endpoint", "", "only endpoints whose name contains this substring")
	group := fs.String("group", "", "only endpoints in this group")
	noColor := fs.Bool("no-color", false, "disable ANSI colour")
	if err := fs.Parse(args); err != nil {
		return err
	}

	prec, err := lineproto.ParsePrecision(*precision)
	if err != nil {
		return err
	}
	if !isTerminal() {
		return fmt.Errorf("stdout is not a terminal; use `stats` for non-interactive output")
	}
	if files, err := store.List(*dir, *prefix); err != nil {
		return err
	} else if len(files) == 0 {
		return fmt.Errorf("no day files in %s; run `uptime-mon run -data %s` first", *dir, *dir)
	}

	set := series.NewSet(0)
	r := &store.Reader{Dir: *dir, Prefix: *prefix, Precision: prec}

	var mu sync.Mutex
	reload := func(from, to time.Time) error {
		mu.Lock()
		defer mu.Unlock()
		// Size the ring to the window before refilling, so switching to 30d
		// does not silently drop the oldest three quarters of it.
		fresh := series.NewSet(capForWindow(to.Sub(from)))
		err := r.Scan(from, to, func(res model.Result) error {
			if *endpoint != "" && !strings.Contains(strings.ToLower(res.Target), strings.ToLower(*endpoint)) {
				return nil
			}
			if *group != "" && !strings.EqualFold(res.Group, *group) {
				return nil
			}
			fresh.Add(res)
			return nil
		})
		if err != nil {
			return err
		}
		set.Adopt(fresh)
		return nil
	}

	now := time.Now()
	if err := reload(now.Add(-*window), now); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// In read-only mode nothing pushes new samples in, so the periodic reload
	// is what makes the view live while a collector writes alongside.
	go func() {
		t := time.NewTicker(*refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n := time.Now()
				if err := reload(n.Add(-*window), n); err != nil {
					fmt.Fprintf(os.Stderr, "reload: %v\n", err)
				}
			}
		}
	}()

	return tui.Run(ctx, tui.Options{
		Set:     set,
		Reload:  reload,
		Title:   *dir + "  (read-only)",
		Window:  *window,
		Refresh: *refresh,
		ApdexT:  *apdexT,
		Color:   colorEnabled(*noColor, false),
		Status: func() string {
			return fmt.Sprintf("%d endpoints  %s samples in memory",
				set.Len(), thousands(set.Samples()))
		},
	})
}

// capForWindow sizes the per-endpoint ring for a window, assuming a 20-second
// floor on probe intervals and leaving headroom.
func capForWindow(window time.Duration) int {
	n := int(window/(20*time.Second)) + 64
	if n < 512 {
		n = 512
	}
	if n > 200000 {
		n = 200000
	}
	return n
}

package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

func newTarget(url string) model.Target {
	return model.Target{
		Name: url, Kind: model.KindHTTP, URL: url, Method: "GET",
		Accept: model.DefaultAccept(), Timeout: 5 * time.Second,
		MaxRedirects: 10, MaxBody: 1 << 20, Enabled: true,
	}
}

func TestHTTPStatusAndTimings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, "hello world")
		case "/500":
			w.WriteHeader(500)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, "late")
		}
	}))
	defer srv.Close()

	p := New(DefaultOptions())
	defer p.Close()
	ctx := context.Background()

	t.Run("up", func(t *testing.T) {
		r, ok := p.Probe(ctx, newTarget(srv.URL+"/ok"))
		if !ok || r.Status != model.StatusUp {
			t.Fatalf("got %+v", r)
		}
		if r.Code != 200 || r.Bytes != 11 || r.Err != "" {
			t.Fatalf("got %+v", r)
		}
		if r.RTT <= 0 || r.TTFB <= 0 {
			t.Fatalf("timings not recorded: %+v", r)
		}
	})

	t.Run("bad status", func(t *testing.T) {
		r, _ := p.Probe(ctx, newTarget(srv.URL+"/500"))
		if r.Status != model.StatusDown || r.Code != 500 {
			t.Fatalf("got %+v", r)
		}
		if !strings.Contains(r.Err, "status 500") {
			t.Fatalf("err = %q", r.Err)
		}
	})

	t.Run("accepts custom range", func(t *testing.T) {
		tg := newTarget(srv.URL + "/500")
		tg.Accept = model.StatusRanges{{Lo: 500, Hi: 500}}
		r, _ := p.Probe(ctx, tg)
		if r.Status != model.StatusUp {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		tg := newTarget(srv.URL + "/slow")
		tg.Timeout = 50 * time.Millisecond
		r, _ := p.Probe(ctx, tg)
		if r.Status != model.StatusDown || r.Err != "timeout" {
			t.Fatalf("got %+v", r)
		}
	})
}

func TestKeyword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "status: healthy\nversion: 3")
	}))
	defer srv.Close()

	p := New(DefaultOptions())
	defer p.Close()

	tg := newTarget(srv.URL)
	tg.Kind = model.KindKeyword
	tg.Keyword = "healthy"
	if r, _ := p.Probe(context.Background(), tg); r.Status != model.StatusUp {
		t.Fatalf("expected up, got %+v", r)
	}

	tg.Keyword = "unhealthy"
	r, _ := p.Probe(context.Background(), tg)
	if r.Status != model.StatusDown || !strings.Contains(r.Err, "keyword missing") {
		t.Fatalf("got %+v", r)
	}

	tg.InvertKeyword = true
	if r, _ := p.Probe(context.Background(), tg); r.Status != model.StatusUp {
		t.Fatalf("inverted keyword should pass, got %+v", r)
	}
}

func TestRedirectPolicy(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			fmt.Fprint(w, "ok")
			return
		}
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	}))
	defer srv.Close()

	p := New(DefaultOptions())
	defer p.Close()

	tg := newTarget(srv.URL + "/start")
	if r, _ := p.Probe(context.Background(), tg); r.Status != model.StatusUp || r.Code != 200 {
		t.Fatalf("following redirects: got %+v", r)
	}

	// redirects=0 means "do not follow"; the 302 itself is then the verdict,
	// which the default 200-299 range rejects.
	tg.MaxRedirects = 0
	r, _ := p.Probe(context.Background(), tg)
	if r.Status != model.StatusDown || r.Code != 302 {
		t.Fatalf("not following: got %+v", r)
	}
}

func TestUpsideDownAndRetries(t *testing.T) {
	p := New(DefaultOptions())
	defer p.Close()

	// Nothing is listening on this port, so the probe fails fast.
	tg := newTarget("http://127.0.0.1:1")
	tg.Timeout = time.Second

	r, _ := p.Probe(context.Background(), tg)
	if r.Status != model.StatusDown {
		t.Fatalf("got %+v", r)
	}

	tg.UpsideDown = true
	if r, _ := p.Probe(context.Background(), tg); r.Status != model.StatusUp {
		t.Fatalf("upside-down should report up, got %+v", r)
	}

	tg.UpsideDown = false
	tg.Retries = 2
	tg.RetryInterval = time.Millisecond
	r, _ = p.Probe(context.Background(), tg)
	if r.Attempt != 3 {
		t.Fatalf("expected 3 attempts, got %d", r.Attempt)
	}
}

func TestTCPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	host, portStr, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	p := New(DefaultOptions())
	defer p.Close()

	tg := model.Target{
		Name: "tcp", Kind: model.KindTCP, Host: host, Port: port,
		Timeout: 2 * time.Second,
	}
	if r, _ := p.Probe(context.Background(), tg); r.Status != model.StatusUp || r.RTT <= 0 {
		t.Fatalf("got %+v", r)
	}

	tg.Port = 1
	if r, _ := p.Probe(context.Background(), tg); r.Status != model.StatusDown {
		t.Fatalf("expected down on closed port, got %+v", r)
	}
}

func TestCancelledContextStops(t *testing.T) {
	p := New(DefaultOptions())
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := p.Probe(ctx, newTarget("http://127.0.0.1:1")); ok {
		t.Fatal("expected ok=false on cancelled context")
	}
}

// Package probe performs a single observation of a target and returns a
// model.Result. It owns all network behaviour: timeouts, retries, TLS policy,
// redirect policy and the timing breakdown.
//
// Everything here is stateless per call except the shared http.Transport pool,
// which exists so that connection reuse (and therefore the memory and fd cost
// of watching 90 endpoints) stays bounded and predictable.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// Options configures the shared prober.
type Options struct {
	// KeepAlive reuses TCP/TLS connections between probes. On by default: it
	// keeps CPU and handshake load low, at the cost of DNS/connect/TLS timings
	// being reported only on the first probe of each connection.
	KeepAlive bool

	// MaxIdlePerHost bounds the idle connection pool. One is enough when each
	// endpoint is probed on its own schedule.
	MaxIdlePerHost int

	// IdleTimeout closes pooled connections that go unused, so a 60s interval
	// does not pin a socket open forever.
	IdleTimeout time.Duration

	// DialTimeout and TLSTimeout bound the sub-phases; the per-target Timeout
	// still bounds the whole attempt.
	DialTimeout time.Duration
	TLSTimeout  time.Duration

	// Resolver overrides DNS, e.g. "1.1.1.1:53". Empty uses the system resolver.
	Resolver string

	// DisableHTTP2 forces HTTP/1.1, which some minimal endpoints handle better.
	DisableHTTP2 bool
}

// DefaultOptions returns the settings used unless flags override them.
func DefaultOptions() Options {
	return Options{
		KeepAlive:      true,
		MaxIdlePerHost: 1,
		IdleTimeout:    90 * time.Second,
		DialTimeout:    10 * time.Second,
		TLSTimeout:     10 * time.Second,
	}
}

// Prober executes probes. It is safe for concurrent use.
type Prober struct {
	opts Options

	mu         sync.Mutex
	transports map[bool]*http.Transport // keyed by "insecure TLS"
	dialer     *net.Dialer
}

// New returns a Prober. Call Close when done to release idle connections.
func New(opts Options) *Prober {
	d := &net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}
	if opts.Resolver != "" {
		addr := opts.Resolver
		if !strings.Contains(addr, ":") {
			addr = net.JoinHostPort(addr, "53")
		}
		d.Resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: opts.DialTimeout}).DialContext(ctx, network, addr)
			},
		}
	}
	return &Prober{
		opts:       opts,
		transports: make(map[bool]*http.Transport, 2),
		dialer:     d,
	}
}

// Close releases pooled connections.
func (p *Prober) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, tr := range p.transports {
		tr.CloseIdleConnections()
	}
}

func (p *Prober) transport(insecure bool) *http.Transport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if tr, ok := p.transports[insecure]; ok {
		return tr
	}
	tr := &http.Transport{
		DialContext:           p.dialer.DialContext,
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   p.opts.MaxIdlePerHost,
		IdleConnTimeout:       p.opts.IdleTimeout,
		TLSHandshakeTimeout:   p.opts.TLSTimeout,
		ExpectContinueTimeout: time.Second,
		DisableKeepAlives:     !p.opts.KeepAlive,
		ForceAttemptHTTP2:     !p.opts.DisableHTTP2,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // opt-in per target
	}
	p.transports[insecure] = tr
	return tr
}

// Probe observes t once, honouring its retry policy, and always returns a
// Result — network failure is data, not an error to propagate. The only way to
// get no Result is a cancelled context, signalled by ok=false.
func (p *Prober) Probe(ctx context.Context, t model.Target) (model.Result, bool) {
	attempts := t.Retries + 1
	var r model.Result
	for i := 1; i <= attempts; i++ {
		if ctx.Err() != nil {
			return r, false
		}
		r = p.once(ctx, t)
		r.Attempt = i
		if r.Status == model.StatusUp {
			return r, true
		}
		if i < attempts {
			// A retry only makes sense after a pause; otherwise we just
			// hammer a host that is already struggling.
			select {
			case <-ctx.Done():
				return r, false
			case <-time.After(t.RetryInterval):
			}
		}
	}
	return r, true
}

// once performs exactly one attempt.
func (p *Prober) once(parent context.Context, t model.Target) model.Result {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	r := model.Result{
		Target: t.Name,
		Group:  t.Group,
		Kind:   t.Kind,
		Host:   t.Host,
		At:     time.Now(),
	}

	switch t.Kind {
	case model.KindPing:
		p.doPing(ctx, t, &r)
	case model.KindTCP:
		p.doTCP(ctx, t, &r)
	default:
		p.doHTTP(ctx, t, &r)
	}

	// UpsideDown inverts the verdict: used for endpoints that are supposed to
	// be unreachable (a firewall rule you want to stay in place).
	if t.UpsideDown {
		if r.Status == model.StatusUp {
			r.Status = model.StatusDown
			if r.Err == "" {
				r.Err = "up but expected down"
			}
		} else {
			r.Status = model.StatusUp
			r.Err = ""
		}
	}
	return r
}

// doTCP measures a plain connection establishment.
func (p *Prober) doTCP(ctx context.Context, t model.Target, r *model.Result) {
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	start := time.Now()
	conn, err := p.dialer.DialContext(ctx, "tcp", addr)
	r.RTT = time.Since(start)
	if err != nil {
		r.Status = model.StatusDown
		r.Err = classify(err)
		return
	}
	_ = conn.Close()
	r.Connect = r.RTT
	r.Status = model.StatusUp
}

// classify reduces Go's verbose network errors to short, stable strings.
// Stability matters: the stats engine groups failures by this text, and
// "dial tcp 1.2.3.4:443: i/o timeout" and "dial tcp 5.6.7.8:443: i/o timeout"
// are the same failure mode for a human reading a summary.
func classify(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "dns: no such host"
		}
		return "dns: " + strings.TrimSuffix(dnsErr.Err, ".")
	}

	var tlsCert *tls.CertificateVerificationError
	if errors.As(err, &tlsCert) {
		return "tls: certificate verification failed"
	}
	var tlsAlert tls.RecordHeaderError
	if errors.As(err, &tlsAlert) {
		return "tls: handshake failed"
	}

	msg := err.Error()
	// Strip the address-specific prefixes Go prepends, keeping the cause.
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		tail := msg[i+2:]
		for _, known := range []string{
			"connection refused", "connection reset by peer", "no route to host",
			"network is unreachable", "host is unreachable", "i/o timeout",
			"broken pipe", "no such host",
		} {
			if tail == known {
				if known == "i/o timeout" {
					return "timeout"
				}
				return known
			}
		}
	}
	if strings.Contains(msg, "x509:") {
		return "tls: certificate error"
	}
	if strings.Contains(msg, "tls:") {
		return "tls: handshake failed"
	}
	if strings.Contains(msg, "stopped after") && strings.Contains(msg, "redirect") {
		return "too many redirects"
	}
	// Last resort: keep it short so it stays readable in a table cell.
	msg = strings.TrimSpace(msg)
	if len(msg) > 120 {
		msg = msg[:117] + "..."
	}
	return msg
}

package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// doHTTP issues one request and fills in the timing breakdown.
//
// RTT is measured to the end of the body read (capped at MaxBody), not to the
// first byte. A page that returns headers instantly and then stalls is not a
// healthy page, and TTFB is recorded separately for anyone who wants to
// separate "server thinking" from "bytes on the wire".
func (p *Prober) doHTTP(ctx context.Context, t model.Target, r *model.Result) {
	method := t.Method
	if method == "" {
		method = http.MethodGet
	}

	var body io.Reader
	if t.Body != "" {
		body = strings.NewReader(t.Body)
	}

	req, err := http.NewRequestWithContext(ctx, method, t.URL, body)
	if err != nil {
		r.Status = model.StatusDown
		r.Err = "bad request: " + err.Error()
		return
	}

	ua := t.UserAgent
	if ua == "" {
		ua = "uptime-mon/1.0"
	}
	req.Header.Set("User-Agent", ua)
	// Ask for an unconditional, uncached answer: a monitor that measures a
	// proxy's cache is not measuring the origin.
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	for _, h := range t.Headers {
		if strings.EqualFold(h.Key, "Host") {
			req.Host = h.Value
			continue
		}
		req.Header.Set(h.Key, h.Value)
	}

	var (
		start                            = time.Now()
		dnsStart, connStart, tlsStart    time.Time
		dnsDur, connDur, tlsDur, ttfbDur time.Duration
		reusedConn                       bool
	)
	trace := &httptrace.ClientTrace{
		GotConn:  func(info httptrace.GotConnInfo) { reusedConn = info.Reused },
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				dnsDur = time.Since(dnsStart)
			}
		},
		ConnectStart: func(_, _ string) {
			if connStart.IsZero() {
				connStart = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			if err == nil && !connStart.IsZero() {
				connDur = time.Since(connStart)
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil && !tlsStart.IsZero() {
				tlsDur = time.Since(tlsStart)
			}
		},
		GotFirstResponseByte: func() { ttfbDur = time.Since(start) },
	}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))

	client := &http.Client{
		Transport:     p.transport(t.Insecure),
		CheckRedirect: redirectPolicy(t.MaxRedirects),
	}

	resp, err := client.Do(req)
	if err != nil {
		r.RTT = time.Since(start)
		r.DNS, r.Connect, r.TLS, r.TTFB = dnsDur, connDur, tlsDur, ttfbDur
		r.Status = model.StatusDown
		r.Err = classify(err)
		return
	}
	defer resp.Body.Close()

	maxBody := t.MaxBody
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	// Keyword checks need the bytes; plain status checks only need them
	// counted, so the body is drained either way to let the connection be
	// reused instead of being torn down mid-stream.
	var bodyText string
	if t.Keyword != "" {
		buf, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		r.Bytes = int64(len(buf))
		bodyText = string(buf)
		if rerr != nil && r.Err == "" {
			r.RTT = time.Since(start)
			r.Status = model.StatusDown
			r.Err = "body read: " + classify(rerr)
			return
		}
	} else {
		n, rerr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		r.Bytes = n
		if rerr != nil {
			r.RTT = time.Since(start)
			r.Status = model.StatusDown
			r.Err = "body read: " + classify(rerr)
			return
		}
	}

	r.RTT = time.Since(start)
	r.DNS, r.Connect, r.TLS, r.TTFB = dnsDur, connDur, tlsDur, ttfbDur
	if reusedConn {
		// Zero these explicitly: on a reused connection they were not measured
		// this time round, and reporting a stale 0 as "0.000 ms DNS" would be
		// read as "DNS was instant" rather than "DNS did not happen".
		r.DNS, r.Connect, r.TLS = 0, 0, 0
	}
	r.Code = resp.StatusCode

	accept := t.Accept
	if len(accept) == 0 {
		accept = model.DefaultAccept()
	}
	if !accept.Contains(resp.StatusCode) {
		r.Status = model.StatusDown
		r.Err = fmt.Sprintf("status %d (want %s)", resp.StatusCode, accept)
		return
	}

	if t.Keyword != "" {
		found := strings.Contains(bodyText, t.Keyword)
		if found == t.InvertKeyword {
			r.Status = model.StatusDown
			if t.InvertKeyword {
				r.Err = "keyword present: " + truncate(t.Keyword, 40)
			} else {
				r.Err = "keyword missing: " + truncate(t.Keyword, 40)
			}
			return
		}
	}

	r.Status = model.StatusUp
}

// redirectPolicy converts a redirect budget into net/http's callback form.
func redirectPolicy(max int) func(*http.Request, []*http.Request) error {
	if max <= 0 {
		// Do not follow: return the 3xx as the final response so the accepted
		// status codes decide whether a redirect counts as healthy.
		return func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return func(_ *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("stopped after %d redirects", max)
		}
		return nil
	}
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "..."
}

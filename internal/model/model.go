// Package model holds the small set of types shared by every other package:
// what we probe, and what a probe produced. Nothing here imports anything but
// the standard library, so the collector, the store and the TUI can all agree
// on shapes without dragging dependencies across package boundaries.
package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind is the probe family for a target.
type Kind string

const (
	KindHTTP    Kind = "http"    // status-code check
	KindKeyword Kind = "keyword" // status-code check plus body substring
	KindPing    Kind = "ping"    // ICMP echo, with a TCP-connect fallback
	KindTCP     Kind = "tcp"     // plain TCP connect
)

// Target is one thing we watch. It is the parsed form of a single non-comment
// line in endpoints.txt, and the import target for an Uptime Kuma monitor.
type Target struct {
	Name    string // display name; defaults to URL when unset
	Group   string // free-form bucket used for filtering and TUI grouping
	Kind    Kind
	URL     string // full URL for http/keyword
	Host    string // hostname for ping, host:port for tcp
	Port    int    // tcp only
	Method  string // http verb, default GET
	Headers []Header
	Body    string

	Keyword       string // keyword kind: substring that must appear in the body
	InvertKeyword bool   // ...or must NOT appear

	Accept        StatusRanges // acceptable status codes, default 200-299
	Interval      time.Duration
	Timeout       time.Duration
	Retries       int           // extra attempts before declaring DOWN
	RetryInterval time.Duration // pause between those attempts

	MaxRedirects int  // 0 disables following
	Insecure     bool // skip TLS verification
	UpsideDown   bool // invert the final verdict
	Enabled      bool
	UserAgent    string
	MaxBody      int64 // cap on body bytes read for keyword matching
}

// Header is a single outgoing request header.
type Header struct {
	Key   string
	Value string
}

// Status is the outcome of one probe.
type Status uint8

const (
	StatusDown Status = iota
	StatusUp
)

func (s Status) String() string {
	if s == StatusUp {
		return "up"
	}
	return "down"
}

// Result is one observation. It is what the collector emits, what the store
// serialises to line protocol, and what the stats engine consumes after
// reading it back. Durations are kept as time.Duration and converted to
// milliseconds only at the serialisation boundary.
type Result struct {
	Target  string    // Target.Name
	Group   string    // Target.Group
	Kind    Kind      //
	Host    string    // hostname, carried as a tag for cross-endpoint grouping
	At      time.Time // when the probe started
	Status  Status
	Code    int           // HTTP status code, 0 for non-HTTP probes
	RTT     time.Duration // total time to a usable answer
	DNS     time.Duration // resolution time, 0 when not measured
	Connect time.Duration
	TLS     time.Duration
	TTFB    time.Duration
	Bytes   int64  // response body bytes read
	Attempt int    // 1 for a first-try success, >1 when retries were spent
	Err     string // short error description, empty when up
}

// StatusRanges is an inclusive set of acceptable HTTP status codes, expressed
// the way Uptime Kuma does it ("200-299", "301", "200-299,404").
type StatusRanges []StatusRange

// StatusRange is one inclusive lo..hi span.
type StatusRange struct{ Lo, Hi int }

// DefaultAccept is the range applied when a target does not specify one.
func DefaultAccept() StatusRanges { return StatusRanges{{200, 299}} }

// Contains reports whether code falls in any range.
func (r StatusRanges) Contains(code int) bool {
	for _, x := range r {
		if code >= x.Lo && code <= x.Hi {
			return true
		}
	}
	return false
}

func (r StatusRanges) String() string {
	parts := make([]string, 0, len(r))
	for _, x := range r {
		if x.Lo == x.Hi {
			parts = append(parts, strconv.Itoa(x.Lo))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", x.Lo, x.Hi))
		}
	}
	return strings.Join(parts, ",")
}

// ParseStatusRanges reads "200-299,301,404" into a StatusRanges.
func ParseStatusRanges(s string) (StatusRanges, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultAccept(), nil
	}
	var out StatusRanges
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		l, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad status code %q", part)
		}
		h := l
		if found {
			h, err = strconv.Atoi(strings.TrimSpace(hi))
			if err != nil {
				return nil, fmt.Errorf("bad status range %q", part)
			}
		}
		if h < l {
			return nil, fmt.Errorf("inverted status range %q", part)
		}
		out = append(out, StatusRange{Lo: l, Hi: h})
	}
	if len(out) == 0 {
		return DefaultAccept(), nil
	}
	return out, nil
}

package probe

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"time"

	"github.com/didvc/uptime-mon/internal/model"
)

// ErrPingUnsupported means this build or this kernel cannot send ICMP echo
// without elevated privileges. Callers turn it into a startup-time decision
// (fall back to a TCP connect probe, or tell the user how to enable ICMP)
// rather than a per-sample error, so the recorded data never quietly changes
// meaning halfway through a day.
var ErrPingUnsupported = errors.New("icmp echo unavailable")

// doPing resolves the host and sends a single ICMP echo request.
func (p *Prober) doPing(ctx context.Context, t model.Target, r *model.Result) {
	start := time.Now()

	resolver := net.DefaultResolver
	if p.dialer.Resolver != nil {
		resolver = p.dialer.Resolver
	}
	ips, err := resolver.LookupIP(ctx, "ip4", t.Host)
	r.DNS = time.Since(start)
	if err != nil || len(ips) == 0 {
		r.RTT = time.Since(start)
		r.Status = model.StatusDown
		if err == nil {
			err = errors.New("no A record")
		}
		r.Err = classify(err)
		return
	}

	deadline := time.Now().Add(t.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	seq := uint16(rand.Intn(1 << 16)) //nolint:gosec // collision-avoidance, not security
	rtt, err := icmpEcho(ips[0], seq, time.Until(deadline))
	r.RTT = time.Since(start)
	if err != nil {
		r.Status = model.StatusDown
		r.Err = classify(err)
		return
	}
	// Report the network round trip, not the resolve-plus-round-trip, since
	// that is what "ping" means to anyone reading the number.
	r.RTT = rtt
	r.Status = model.StatusUp
}

// PingAvailable reports whether ICMP echo can be sent without root. On Linux
// this depends on net.ipv4.ping_group_range covering the current gid.
func PingAvailable() error { return pingProbeSupport() }

// checksum computes the standard internet checksum over b, treating it as a
// sequence of big-endian 16-bit words.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

// echoRequest builds an ICMP type 8 echo request. The id is left at zero
// because the kernel overwrites it on an unprivileged datagram socket; replies
// are matched on the sequence number instead.
func echoRequest(seq uint16, payload []byte) []byte {
	pkt := make([]byte, 8+len(payload))
	pkt[0] = 8 // echo request
	pkt[1] = 0 // code
	pkt[4], pkt[5] = 0, 0
	pkt[6], pkt[7] = byte(seq>>8), byte(seq)
	copy(pkt[8:], payload)
	c := checksum(pkt)
	pkt[2], pkt[3] = byte(c>>8), byte(c)
	return pkt
}

// matchEchoReply reports whether buf is an echo reply carrying seq.
func matchEchoReply(buf []byte, seq uint16) bool {
	if len(buf) < 8 {
		return false
	}
	if buf[0] != 0 { // 0 = echo reply
		return false
	}
	got := uint16(buf[6])<<8 | uint16(buf[7])
	return got == seq
}

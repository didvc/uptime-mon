//go:build linux

package probe

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

// protocolICMP is IPPROTO_ICMP. A SOCK_DGRAM socket on this protocol is the
// unprivileged "ping socket" Linux exposes to gids listed in
// net.ipv4.ping_group_range — no CAP_NET_RAW and no setuid helper needed.
const protocolICMP = 1

func pingProbeSupport() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, protocolICMP)
	if err != nil {
		return fmt.Errorf("%w: %v (try: sysctl -w net.ipv4.ping_group_range=\"0 2147483647\")",
			ErrPingUnsupported, err)
	}
	_ = syscall.Close(fd)
	return nil
}

// icmpEcho sends one echo request to ip and waits for the matching reply.
//
// This uses raw syscalls rather than Go's net package because net.ListenPacket
// has no way to ask for IPPROTO_ICMP. The read is bounded by SO_RCVTIMEO
// instead of the runtime poller, so it parks one OS thread for at most the
// timeout — fine for the handful of ping targets a config like this has.
func icmpEcho(ip net.IP, seq uint16, timeout time.Duration) (time.Duration, error) {
	v4 := ip.To4()
	if v4 == nil {
		return 0, fmt.Errorf("%w: only IPv4 targets are supported", ErrPingUnsupported)
	}
	if timeout <= 0 {
		return 0, syscall.ETIMEDOUT
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, protocolICMP)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrPingUnsupported, err)
	}
	defer syscall.Close(fd)

	tv := syscall.NsecToTimeval(int64(timeout))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return 0, err
	}

	var addr syscall.SockaddrInet4
	copy(addr.Addr[:], v4)

	start := time.Now()
	payload := make([]byte, 16)
	nano := start.UnixNano()
	for i := 0; i < 8; i++ {
		payload[i] = byte(nano >> (8 * (7 - i)))
	}
	if err := syscall.Sendto(fd, echoRequest(seq, payload), 0, &addr); err != nil {
		return 0, err
	}

	buf := make([]byte, 1500)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if err == syscall.EINTR {
				// Interrupted by a signal (Go's preemption does this); the
				// deadline still applies, so just resume waiting.
				if time.Since(start) >= timeout {
					return 0, syscall.ETIMEDOUT
				}
				continue
			}
			if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
				return 0, syscall.ETIMEDOUT
			}
			return 0, err
		}
		if matchEchoReply(buf[:n], seq) {
			return time.Since(start), nil
		}
		// A reply for someone else's sequence number: keep waiting, but do not
		// extend the budget.
		if time.Since(start) >= timeout {
			return 0, syscall.ETIMEDOUT
		}
	}
}

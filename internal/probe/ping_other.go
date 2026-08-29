//go:build !linux

package probe

import (
	"net"
	"time"
)

// On platforms without Linux's unprivileged ping socket, ICMP needs raw socket
// privileges that a monitoring daemon should not be asking for. The collector
// converts this into a TCP-connect probe at startup (see -ping-fallback).

func pingProbeSupport() error { return ErrPingUnsupported }

func icmpEcho(net.IP, uint16, time.Duration) (time.Duration, error) {
	return 0, ErrPingUnsupported
}

package power

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

const (
	defaultAttempts = 3
	replyTimeout    = time.Second
	protocolICMP    = 1
)

var errHostUnreachable error = syscall.EHOSTUNREACH

// ICMP probes with unprivileged ping sockets (net.ipv4.ping_group_range).
// Only silence counts as offline, any local failure is an error.
type ICMP struct {
	Address  string
	Attempts int
}

func (p *ICMP) Offline(ctx context.Context) (bool, error) {
	ip := net.ParseIP(p.Address)
	if ip == nil || ip.To4() == nil {
		return false, fmt.Errorf("invalid IPv4 address %q", p.Address)
	}
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return false, fmt.Errorf("opening ping socket: %w", err)
	}
	defer conn.Close()

	attempts := p.Attempts
	if attempts <= 0 {
		attempts = defaultAttempts
	}
	for seq := 1; seq <= attempts; seq++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		replied, err := ping(conn, ip, seq)
		if err != nil {
			return false, err
		}
		if replied {
			return false, nil
		}
	}
	return true, nil
}

func ping(conn *icmp.PacketConn, ip net.IP, seq int) (bool, error) {
	msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{Seq: seq, Data: []byte("nekromancer")}}
	raw, err := msg.Marshal(nil)
	if err != nil {
		return false, err
	}
	if _, err := conn.WriteTo(raw, &net.UDPAddr{IP: ip}); err != nil {
		if isSilence(err) {
			return false, nil
		}
		return false, fmt.Errorf("sending ping: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(replyTimeout)); err != nil {
		return false, err
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) || (err != nil && isSilence(err)) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("reading ping reply: %w", err)
		}
		from, ok := peer.(*net.UDPAddr)
		if !ok || !from.IP.Equal(ip) {
			continue
		}
		reply, err := icmp.ParseMessage(protocolICMP, buf[:n])
		if err != nil || reply.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		if echo, ok := reply.Body.(*icmp.Echo); ok && echo.Seq == seq {
			return true, nil
		}
	}
}

// isSilence reports whether a socket error means the peer did not answer ARP.
// ENETUNREACH is excluded on purpose: that is our routing, not the host.
func isSilence(err error) bool {
	return errors.Is(err, errHostUnreachable)
}

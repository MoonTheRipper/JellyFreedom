package netnsproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// Dialer dials through a SOCKS5 proxy. It is the caller-side half of this package: the
// orchestrator, running in the host namespace, uses one to reach the internet as though
// it were inside the vpntorrent namespace.
//
// It is written by hand rather than pulled from golang.org/x/net/proxy for two reasons:
// the protocol needed here is one exchange long, and the module must keep building to a
// single static binary with the dependency list it already has.
type Dialer struct {
	// Addr is the proxy's host:port — the namespace's veth address and the proxy port.
	Addr string
	// Timeout bounds the connection to the proxy and the handshake with it. It does not
	// bound the tunnelled connection: see handshakeTimeout on the server side.
	Timeout time.Duration
}

// ErrNoProxy is returned by DialContext when the Dialer has no address configured. It is
// a distinct value so callers can tell "web sources are not set up on this box" apart
// from "the tunnel is down", which need different messages.
var ErrNoProxy = errors.New("no VPN proxy is configured")

// DialContext opens a connection to addr through the proxy.
//
// A host NAME is forwarded as a name, never resolved here. That is the whole point: a
// lookup done in this process would leave from the host's interface, so the user's real
// address would appear in a DNS query for the very site whose video is about to be
// fetched through the tunnel. Resolution happens inside the namespace, where the
// namespace's own resolvers are reached over WireGuard.
func (d Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("netnsproxy: %s is not supported (TCP only)", network)
	}
	if d.Addr == "" {
		return nil, ErrNoProxy
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("netnsproxy: bad destination %q: %w", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("netnsproxy: bad destination port in %q", addr)
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", d.Addr)
	if err != nil {
		return nil, fmt.Errorf("netnsproxy: cannot reach the VPN proxy at %s: %w", d.Addr, err)
	}

	// Every failure past this point must close the connection; a leaked socket here is a
	// leaked socket per playback attempt.
	if err := socksHandshake(ctx, conn, host, uint16(port), timeout); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func socksHandshake(ctx context.Context, conn net.Conn, host string, port uint16, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	// Clear the handshake deadline on success — the caller's connection outlives it.
	defer conn.SetDeadline(time.Time{})

	// Greeting: version, one method, "no authentication".
	if _, err := conn.Write([]byte{socksVersion, 0x01, 0x00}); err != nil {
		return fmt.Errorf("netnsproxy: greeting: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("netnsproxy: reading method selection: %w", err)
	}
	if resp[0] != socksVersion || resp[1] != 0x00 {
		return fmt.Errorf("netnsproxy: proxy refused the no-auth method (got %#x %#x)", resp[0], resp[1])
	}

	req := []byte{socksVersion, cmdConnect, 0x00}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Is4() {
			b := ip.As4()
			req = append(req, atypIPv4)
			req = append(req, b[:]...)
		} else {
			b := ip.As16()
			req = append(req, atypIPv6)
			req = append(req, b[:]...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("netnsproxy: destination name is too long (%d bytes)", len(host))
		}
		req = append(req, atypDomain, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("netnsproxy: sending CONNECT: %w", err)
	}

	head := make([]byte, 4) // VER REP RSV ATYP
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("netnsproxy: reading CONNECT reply: %w", err)
	}
	if head[0] != socksVersion {
		return fmt.Errorf("netnsproxy: bad reply version %#x", head[0])
	}
	if head[1] != repSuccess {
		return fmt.Errorf("netnsproxy: the VPN proxy refused the connection: %s", replyMessage(head[1]))
	}
	// The bound address is not used, but it MUST be consumed: it sits between the reply
	// header and the tunnelled stream, so leaving it in the buffer would prepend garbage
	// to the first bytes the caller reads.
	var skip int
	switch head[3] {
	case atypIPv4:
		skip = 4
	case atypIPv6:
		skip = 16
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fmt.Errorf("netnsproxy: reading bound address length: %w", err)
		}
		skip = int(l[0])
	default:
		return fmt.Errorf("netnsproxy: bad reply address type %#x", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil { // +2 for the port
		return fmt.Errorf("netnsproxy: reading bound address: %w", err)
	}
	return nil
}

// replyMessage turns a SOCKS reply code into something a user could act on. The one that
// matters in practice is repNotAllowed, which on this proxy means exactly one thing.
func replyMessage(code byte) string {
	switch code {
	case repGeneralFailure:
		return "general failure"
	case repNotAllowed:
		return "not allowed — the destination is not a public internet address"
	case repHostUnreachable:
		return "host unreachable (the tunnel may be down)"
	case repCommandNotSupport:
		return "command not supported"
	case repAddrNotSupport:
		return "address type not supported"
	default:
		return fmt.Sprintf("code %#x", code)
	}
}

// ProxyURL renders the address as the socks5:// URL an external tool takes on its
// command line. It returns "" for an unconfigured Dialer, so a caller can tell the tool
// "no proxy" apart from "an empty proxy", which some tools treat as "direct".
func (d Dialer) ProxyURL() string {
	if d.Addr == "" {
		return ""
	}
	// socks5h, not socks5: the "h" tells curl/yt-dlp to send the HOSTNAME to the proxy
	// and let it resolve, rather than resolving locally first. Same leak, same reason as
	// DialContext's comment — and here it is a one-character difference between the DNS
	// query going through the tunnel and going out of the user's home connection.
	return "socks5h://" + d.Addr
}

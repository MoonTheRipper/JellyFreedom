// Package netnsproxy is a SOCKS5 CONNECT proxy whose only purpose is to lend its own
// network namespace to a process running outside it.
//
// # WHY THIS EXISTS
//
// The orchestrator serves the LAN, so it lives in the host namespace. Everything that
// must not carry the user's home IP lives in the `vpntorrent` namespace, behind the
// WireGuard tunnel and its fail-closed kill switch. Torrent traffic already does: the
// orchestrator talks to TorrServer over the veth, and TorrServer does the swarm's
// networking from inside.
//
// Web sources have no TorrServer. The orchestrator itself has to fetch a page, run an
// extractor against it, and then stream the video bytes — outbound connections that
// would otherwise leave from the host's interface with the user's real address on them.
//
// A process cannot enter a network namespace it did not start in without CAP_SYS_ADMIN,
// and granting the service user that is granting it root. So rather than moving the
// caller into the namespace, this moves one socket: a tiny proxy runs INSIDE the
// namespace as its own systemd unit (NetworkNamespacePath=), listens on the namespace's
// end of the veth, and dials on the caller's behalf. Every connection it opens is
// subject to the same kill switch as everything else in there — when the tunnel is down
// the namespace's iptables OUTPUT policy drops the packets and the caller gets a
// failure. It fails closed for free, because it is not a second mechanism.
//
// SOCKS5 rather than an HTTP proxy for one reason: yt-dlp speaks it natively
// (--proxy socks5://host:port), the client side is ~60 lines of Go with no dependency,
// and it is transport-agnostic — so proxying an HTTPS stream needs no certificate games.
//
// # THREAT MODEL
//
// This is an unauthenticated proxy, so "who may use it" is entirely positional and is
// enforced twice:
//
//   - It binds ONLY the namespace's veth address. Not the tunnel interface, so nothing
//     on the VPN side can reach it; not a LAN interface, because it has none.
//   - It accepts connections only from the host's veth address — a /30 with exactly two
//     usable addresses, the other of which is itself. See AllowFrom.
//
// "Where it may go" is enforced once, on the RESOLVED destination address: the public
// internet only. A proxy that dials 127.0.0.1 or 10.42.0.1 on request is a way back into
// the host from anything that can reach it, which is the pivot the namespace exists to
// prevent. Resolution happens here, in this process, so the address that is checked is
// the address that is dialled — a name checked and then re-resolved by the dialler could
// answer differently the second time.
package netnsproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	socksVersion = 0x05
	cmdConnect   = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	repSuccess           = 0x00
	repGeneralFailure    = 0x01
	repNotAllowed        = 0x02
	repHostUnreachable   = 0x04
	repCommandNotSupport = 0x07
	repAddrNotSupport    = 0x08
)

// handshakeTimeout bounds everything up to and including the CONNECT reply. It
// deliberately does NOT bound the tunnelled connection afterwards: a film streams for
// hours over one socket, and a deadline that outlived the handshake would cut playback
// off mid-scene.
const handshakeTimeout = 20 * time.Second

// dialTimeout bounds the outbound connection attempt. With the kill switch active the
// packets are DROPPED rather than refused, so without an explicit timeout a dial against
// a dead tunnel waits out the kernel's SYN retry budget — roughly two minutes of a
// caller staring at nothing.
const dialTimeout = 20 * time.Second

// Server is a SOCKS5 CONNECT proxy. Build one with New.
type Server struct {
	// allowFrom is the set of client addresses permitted to use the proxy. A connection
	// from anything else is closed before a byte is read from it. Empty means "anyone",
	// which is only ever correct in a test.
	allowFrom []netip.Addr

	// dial opens the outbound connection. Overridable for tests; nil means a plain
	// net.Dialer — which, because this process runs inside the namespace, dials through
	// the tunnel.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// allowPrivate disables the public-internet-only destination check. Tests set it,
	// because a test dials a loopback listener. Production must not: see the threat
	// model above.
	allowPrivate bool

	wg sync.WaitGroup
}

// New returns a Server that will accept only from the given client addresses.
//
// It parses the allow-list eagerly so that a typo in a unit file is a startup failure
// with a clear message, rather than a proxy that silently refuses every connection and
// looks like a networking problem.
func New(allowFrom []string) (*Server, error) {
	s := &Server{}
	for _, a := range allowFrom {
		addr, err := netip.ParseAddr(a)
		if err != nil {
			return nil, fmt.Errorf("allowed client address %q: %w", a, err)
		}
		s.allowFrom = append(s.allowFrom, addr.Unmap())
	}
	return s, nil
}

// Serve accepts connections on ln until ctx is cancelled or ln fails, then waits for
// every in-flight tunnel to finish.
//
// A tunnel is NOT cancelled by ctx: the connections this carries are video streams, and
// tearing them down on shutdown would turn a routine restart into a visible playback
// error. Closing the listener stops new ones; the existing ones end when their clients
// hang up.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

// permitted reports whether a client address may use the proxy.
func (s *Server) permitted(a net.Addr) bool {
	if len(s.allowFrom) == 0 {
		return true
	}
	tcp, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	client, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	client = client.Unmap()
	for _, allowed := range s.allowFrom {
		if client == allowed {
			return true
		}
	}
	return false
}

func (s *Server) handle(client net.Conn) {
	defer client.Close()

	if !s.permitted(client.RemoteAddr()) {
		// No SOCKS reply: a caller that is not allowed to use the proxy is not owed a
		// protocol-level answer telling it what this port is.
		slog.Warn("netnsproxy: refused a connection from an address outside the allow-list",
			"remote", client.RemoteAddr().String())
		return
	}

	client.SetDeadline(time.Now().Add(handshakeTimeout))

	if err := readGreeting(client); err != nil {
		slog.Debug("netnsproxy: greeting failed", "err", err)
		return
	}
	host, port, err := readConnectRequest(client)
	if err != nil {
		var se socksError
		if errors.As(err, &se) {
			writeReply(client, se.reply)
		}
		slog.Debug("netnsproxy: request failed", "err", err)
		return
	}

	target, err := s.resolve(host, port)
	if err != nil {
		var se socksError
		rep := byte(repHostUnreachable)
		if errors.As(err, &se) {
			rep = se.reply
		}
		writeReply(client, rep)
		slog.Warn("netnsproxy: refusing to connect", "host", host, "port", port, "err", err)
		return
	}

	dial := s.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	dctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	upstream, err := dial(dctx, "tcp", target)
	if err != nil {
		writeReply(client, repHostUnreachable)
		// The likeliest cause by far is the kill switch doing its job, so this is Info
		// and says so — it is the message someone will be reading when they wonder why
		// nothing plays with the VPN down.
		slog.Info("netnsproxy: outbound connection failed (is the tunnel up?)",
			"target", target, "err", err)
		return
	}
	defer upstream.Close()

	if err := writeReply(client, repSuccess); err != nil {
		return
	}
	// The handshake is over. Clear the deadline: from here this is a byte pipe that may
	// legitimately live for hours.
	client.SetDeadline(time.Time{})

	pipe(client, upstream)
}

// resolve turns a SOCKS destination into a dialable ip:port, refusing anything that is
// not a public internet address.
//
// Names are resolved HERE rather than handed to the dialler, so that the address checked
// is the address dialled. A name that is checked and then re-resolved one layer down can
// answer differently the second time — the DNS rebinding shape — and the whole value of
// the check is that it cannot.
func (s *Server) resolve(host string, port uint16) (string, error) {
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip.Unmap()}
	} else {
		names, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", host)
		if err != nil {
			return "", fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, a := range names {
			addrs = append(addrs, a.Unmap())
		}
	}
	for _, a := range addrs {
		if s.allowPrivate || isPublic(a) {
			return net.JoinHostPort(a.String(), strconv.Itoa(int(port))), nil
		}
	}
	return "", socksError{reply: repNotAllowed,
		msg: fmt.Sprintf("%q resolves only to non-public addresses", host)}
}

// isPublic reports whether an address is one this proxy is willing to reach. The list is
// what it excludes, not what it includes: loopback, RFC1918 and its v6 equivalent,
// link-local (which covers 169.254.169.254, the cloud metadata address), multicast, the
// unspecified address, and anything the standard library flags as interface-local.
func isPublic(a netip.Addr) bool {
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsMulticast() ||
		a.IsInterfaceLocalMulticast() {
		return false
	}
	// Neither IsPrivate nor anything else in net/netip covers the shared-address space
	// carriers use for NAT (RFC 6598) or the benchmarking range, and both are reachable
	// enough to be worth excluding by hand.
	if a.Is4() {
		b := a.As4()
		if b[0] == 100 && b[1] >= 64 && b[1] <= 127 { // 100.64.0.0/10
			return false
		}
		if b[0] == 198 && (b[1] == 18 || b[1] == 19) { // 198.18.0.0/15
			return false
		}
	}
	return true
}

// socksError carries the SOCKS reply code that should be sent for a failure, so that the
// wire-level answer is decided where the reason is known.
type socksError struct {
	reply byte
	msg   string
}

func (e socksError) Error() string { return e.msg }

// readGreeting performs the method-selection exchange, accepting only "no
// authentication". There is nothing to authenticate: access is positional, checked
// before this is called.
func readGreeting(c net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if head[0] != socksVersion {
		return fmt.Errorf("unsupported SOCKS version %d", head[0])
	}
	n := int(head[1])
	if n == 0 {
		return errors.New("greeting offered no auth methods")
	}
	if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
		return fmt.Errorf("read auth methods: %w", err)
	}
	// 0x00 = no authentication required. Offered unconditionally: a client that cannot
	// do it will close, and there is no method here worth negotiating over.
	if _, err := c.Write([]byte{socksVersion, 0x00}); err != nil {
		return fmt.Errorf("write method selection: %w", err)
	}
	return nil
}

// readConnectRequest parses one SOCKS5 request and returns its destination. Errors carry
// the reply code the caller should send back.
func readConnectRequest(c net.Conn) (host string, port uint16, err error) {
	head := make([]byte, 4) // VER CMD RSV ATYP
	if _, err := io.ReadFull(c, head); err != nil {
		return "", 0, fmt.Errorf("read request: %w", err)
	}
	if head[0] != socksVersion {
		return "", 0, socksError{repGeneralFailure, fmt.Sprintf("unsupported SOCKS version %d", head[0])}
	}
	if head[1] != cmdConnect {
		// BIND and UDP ASSOCIATE both ask this process to LISTEN inside the namespace on
		// behalf of an outsider. Nothing here needs that, and it is a far larger thing to
		// expose than a single outbound connection.
		return "", 0, socksError{repCommandNotSupport, fmt.Sprintf("command %d is not supported (CONNECT only)", head[1])}
	}
	switch head[3] {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", 0, fmt.Errorf("read IPv4 destination: %w", err)
		}
		host = netip.AddrFrom4([4]byte(b)).String()
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", 0, fmt.Errorf("read IPv6 destination: %w", err)
		}
		host = netip.AddrFrom16([16]byte(b)).String()
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", 0, fmt.Errorf("read domain length: %w", err)
		}
		if l[0] == 0 {
			return "", 0, socksError{repAddrNotSupport, "empty destination name"}
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return "", 0, fmt.Errorf("read domain: %w", err)
		}
		host = string(b)
	default:
		return "", 0, socksError{repAddrNotSupport, fmt.Sprintf("address type %d is not supported", head[3])}
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return "", 0, fmt.Errorf("read port: %w", err)
	}
	port = binary.BigEndian.Uint16(pb)
	if port == 0 {
		return "", 0, socksError{repAddrNotSupport, "port 0 is not a destination"}
	}
	return host, port, nil
}

// writeReply sends a SOCKS5 reply with a zero bound address.
//
// The bound address is what the proxy's end of the outbound connection is, and it is
// reported as 0.0.0.0:0 on purpose: no client here needs it, and a truthful value would
// hand the caller the namespace's tunnel address for no reason. RFC 1928 permits it and
// every client in practice ignores the field on a CONNECT reply.
func writeReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{socksVersion, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

// pipe copies in both directions and returns once either side is done.
//
// Each direction half-closes its destination when its source ends, so an upstream that
// signals EOF is passed through as EOF rather than as a stalled connection — which for a
// media stream is the difference between a player seeing the end of the file and a
// player hanging on the last byte.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

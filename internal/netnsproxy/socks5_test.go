package netnsproxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// echoServer is the thing on the far side of the proxy: it reads a line and writes it
// back, so a test can prove that bytes crossed in both directions rather than merely
// that a connection was accepted.
func echoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// startProxy runs a Server on loopback and returns its address. allowPrivate is on
// because every test destination here is 127.0.0.1, which production must refuse.
func startProxy(t *testing.T, configure func(*Server)) string {
	t.Helper()
	s, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.allowPrivate = true
	if configure != nil {
		configure(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Serve(ctx, ln)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String()
}

func TestRoundTripThroughProxy(t *testing.T) {
	echo := echoServer(t)
	proxy := startProxy(t, nil)

	d := Dialer{Addr: proxy, Timeout: 5 * time.Second}
	conn, err := d.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("dial through proxy: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 6)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello\n" {
		t.Fatalf("echoed %q, want %q", buf, "hello\n")
	}
}

// A hostname must reach the proxy AS A NAME. If the client resolved it locally, the DNS
// query would leave from the host interface — the exact leak the proxy exists to close —
// so this asserts on the wire bytes, not on the outcome.
func TestClientForwardsHostnameUnresolved(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	seen := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.ReadFull(c, make([]byte, 3))     // greeting
		c.Write([]byte{socksVersion, 0x00}) // no auth
		head := make([]byte, 4)
		io.ReadFull(c, head)
		l := make([]byte, 1)
		io.ReadFull(c, l)
		name := make([]byte, int(l[0]))
		io.ReadFull(c, name)
		io.ReadFull(c, make([]byte, 2)) // port
		seen <- append([]byte{head[3]}, name...)
		c.Write([]byte{socksVersion, repSuccess, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	}()

	d := Dialer{Addr: ln.Addr().String(), Timeout: 5 * time.Second}
	conn, err := d.DialContext(context.Background(), "tcp", "example.invalid:443")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case got := <-seen:
		if got[0] != atypDomain {
			t.Fatalf("address type %#x, want atypDomain — the client resolved the name itself", got[0])
		}
		if string(got[1:]) != "example.invalid" {
			t.Fatalf("forwarded %q, want %q", got[1:], "example.invalid")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never saw a request")
	}
}

func TestRefusesClientsOutsideTheAllowList(t *testing.T) {
	// 10.42.0.1 is the host's veth address in production. A test connecting from
	// loopback is therefore not on the list, which is the case being asserted.
	proxy := startProxy(t, func(s *Server) {
		s.allowFrom = []netip.Addr{netip.MustParseAddr("10.42.0.1")}
	})
	d := Dialer{Addr: proxy, Timeout: 3 * time.Second}
	_, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:9")
	if err == nil {
		t.Fatal("a client outside the allow-list was served")
	}
}

// The destination check is the one that stops the proxy being a way back into the host.
// With allowPrivate off — as in production — loopback must be refused.
func TestRefusesNonPublicDestinations(t *testing.T) {
	echo := echoServer(t)
	proxy := startProxy(t, func(s *Server) { s.allowPrivate = false })

	d := Dialer{Addr: proxy, Timeout: 3 * time.Second}
	_, err := d.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err == nil {
		t.Fatal("the proxy connected to a loopback destination")
	}
	if !strings.Contains(err.Error(), "not a public internet address") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

func TestRefusesNonConnectCommands(t *testing.T) {
	proxy := startProxy(t, nil)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{socksVersion, 0x01, 0x00})
	io.ReadFull(c, make([]byte, 2))
	// BIND, with an IPv4 destination.
	c.Write([]byte{socksVersion, 0x02, 0x00, atypIPv4, 127, 0, 0, 1, 0, 80})
	reply := make([]byte, 2)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[1] != repCommandNotSupport {
		t.Fatalf("reply %#x, want repCommandNotSupport", reply[1])
	}
}

func TestIsPublic(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"1.1.1.1", true},
		{"93.184.216.34", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.42.0.1", false},    // the host, across the veth
		{"192.168.1.10", false}, // the LAN Jellyfin sits on
		{"172.16.0.1", false},
		{"169.254.169.254", false}, // cloud metadata
		{"100.64.0.1", false},      // CGNAT — also Tailscale's range
		{"198.18.0.1", false},
		{"0.0.0.0", false},
		{"224.0.0.1", false},
	} {
		if got := isPublic(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("isPublic(%s) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestDialerWithoutAddressIsADistinctError(t *testing.T) {
	_, err := Dialer{}.DialContext(context.Background(), "tcp", "example.com:443")
	if err != ErrNoProxy {
		t.Fatalf("err = %v, want ErrNoProxy — callers branch on it to explain themselves", err)
	}
}

package fleettest

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// Proxy is a TCP proxy in front of a Server, for breaking the connection the
// way a network does. Point the client at Proxy.WSURL instead of Server.WSURL.
type Proxy struct {
	// WSURL is the client endpoint through the proxy.
	WSURL string

	target string
	ln     net.Listener

	mu    sync.Mutex
	down  bool
	links map[*link]struct{}
}

// link is one proxied connection: client <-> proxy <-> server.
type link struct {
	client, server net.Conn
	// stalled: bytes from the client are read and thrown away.
	stalled atomic.Bool
}

func (l *link) close() {
	l.client.Close()
	l.server.Close()
}

// Proxy starts a proxy in front of the server, stopped when the test ends.
func (s *Server) Proxy(t testing.TB) *Proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{
		WSURL:  "ws://" + ln.Addr().String() + "/ws",
		target: s.Addr,
		ln:     ln,
		links:  map[*link]struct{}{},
	}
	go p.accept()
	t.Cleanup(func() {
		ln.Close()
		p.Drop()
	})
	return p
}

func (p *Proxy) accept() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.serve(client)
	}
}

func (p *Proxy) serve(client net.Conn) {
	p.mu.Lock()
	down := p.down
	p.mu.Unlock()
	if down {
		client.Close()
		return
	}
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	l := &link{client: client, server: server}
	p.mu.Lock()
	p.links[l] = struct{}{}
	p.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { pipe(server, client, &l.stalled); done <- struct{}{} }()
	go func() { pipe(client, server, nil); done <- struct{}{} }()
	<-done
	l.close()
	<-done

	p.mu.Lock()
	delete(p.links, l)
	p.mu.Unlock()
}

func pipe(dst, src net.Conn, stalled *atomic.Bool) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && (stalled == nil || !stalled.Load()) {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Drop cuts every connection going through the proxy right now. New ones are
// still accepted.
func (p *Proxy) Drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for l := range p.links {
		l.close()
	}
}

// SetDown(true) is an outage: every connection is cut and new ones are closed
// as soon as they arrive. SetDown(false) ends it.
func (p *Proxy) SetDown(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
	if down {
		p.Drop()
	}
}

// StallUpstream makes the connections open right now one-way: the server still
// reaches the client, but nothing the client sends arrives (so its heartbeats
// lapse). Connections made afterwards are not affected.
func (p *Proxy) StallUpstream() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for l := range p.links {
		l.stalled.Store(true)
	}
}

// Connections is how many connections are going through the proxy right now.
func (p *Proxy) Connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.links)
}

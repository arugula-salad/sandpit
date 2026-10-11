package substrate

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/store"
)

// The guest's channel to sandpitd (the in-guest API, sprite-env, service
// reports): there is no vsock into an actor, so while a sandbox with an agent
// runs, the engine keeps a few streams parked in it (GET /internal/hostchan,
// agent.ReverseHost) and serves the guest API on each the guest takes.

// hostChanIdle is how many streams are kept parked.
const hostChanIdle = 2

// SetGuestAPI installs the handler the guest channel serves, as the
// Firecracker engine's does.
func (e *Engine) SetGuestAPI(f func(store.Record, *engine.GuestChan) http.Handler) {
	e.mu.Lock()
	e.guestAPI = f
	e.mu.Unlock()
}

// keepHostChannel runs while g is the running guest: it parks streams in the
// agent and serves the guest API on the ones taken. The first stream after a
// resume tells the agent to drop what it had parked, which went with the
// suspend.
func (e *Engine) keepHostChannel(g *guest, rec store.Record) {
	e.mu.Lock()
	f := e.guestAPI
	e.mu.Unlock()
	if f == nil || g.agentToken == "" {
		return
	}
	handler := f(rec, &engine.GuestChan{})
	slots := make(chan struct{}, hostChanIdle)
	reset := true
	for {
		select {
		case <-g.quit:
			return
		case slots <- struct{}{}:
		}
		c, err := g.openHostChan(reset)
		if err != nil {
			<-slots
			select {
			case <-g.quit:
				return
			case <-time.After(time.Second):
			}
			continue
		}
		reset = false
		go serveHostChan(c, handler, func() { <-slots }, g.quit)
	}
}

// openHostChan opens one stream and parks it in the agent.
func (g *guest) openHostChan(reset bool) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := g.Dial(ctx)
	if err != nil {
		return nil, err
	}
	q := ""
	if reset {
		q = "?reset=1"
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(c, "GET /internal/hostchan%s HTTP/1.1\r\nHost: agent\r\nConnection: Upgrade\r\nUpgrade: sandpit-hostchan\r\n\r\n", q)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Close()
		return nil, fmt.Errorf("substrate: host channel: %s", resp.Status)
	}
	c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, r: br}, nil
}

// serveHostChan serves the guest API on one parked stream. taken frees its
// slot once the guest starts using it (or it dies unused), so another is
// parked in its place.
func serveHostChan(c net.Conn, h http.Handler, taken func(), quit <-chan struct{}) {
	tc := &takenConn{Conn: c, taken: taken}
	defer tc.release()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Minute}
	done := make(chan struct{})
	go func() {
		select {
		case <-quit:
			c.Close()
		case <-done:
		}
	}()
	srv.Serve(&oneConnListener{c: tc})
	close(done)
}

// takenConn calls taken on its first read with data, or when it is closed.
type takenConn struct {
	net.Conn
	once  sync.Once
	taken func()
}

func (c *takenConn) release() { c.once.Do(c.taken) }

func (c *takenConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 || err != nil {
		c.release()
	}
	return n, err
}

func (c *takenConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// oneConnListener hands out one connection, then waits until it is closed.
type oneConnListener struct {
	c      net.Conn
	once   sync.Once
	closed chan struct{}
	mu     sync.Mutex
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed == nil {
		l.closed = make(chan struct{})
	}
	l.mu.Unlock()
	var c net.Conn
	l.once.Do(func() { c = &closeNotifyConn{Conn: l.c, closed: l.closed} })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.c.LocalAddr() }

type closeNotifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeNotifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

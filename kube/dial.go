package kube

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/arugula-salad/sandpit/internal/agent"
)

// guest is a running sandbox's pod as the front ends reach it (engine.Guest):
// each Dial is a fresh stream to its agent, authenticated with the token.
type guest struct {
	pod    string
	podUID types.UID
	token  string
	dial   func(ctx context.Context) (net.Conn, error)
}

func (e *Engine) guestFor(p *corev1.Pod, token string) *guest {
	g := &guest{pod: p.Name, podUID: p.UID, token: token}
	port := strconv.Itoa(e.opts.AgentPort)
	switch e.opts.Dial {
	case DialPortForward:
		g.dial = func(ctx context.Context) (net.Conn, error) { return e.portForward(ctx, p.Name, port) }
	default:
		addr := net.JoinHostPort(p.Status.PodIP, port)
		g.dial = func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	return g
}

// Dial opens a stream to the agent: the transport's connection, then the
// token handshake, bounded by ctx's deadline or five seconds as a vsock
// dial is (vmm.Machine.Dial).
func (g *guest) Dial(ctx context.Context) (net.Conn, error) {
	c, err := g.dial(ctx)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	} else {
		c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if err := agent.Handshake(c, g.token); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return c, nil
}

// portForward opens one stream to port in pod through the API server's
// pods/portforward (SPDY), as kubectl port-forward does for each connection
// it accepts.
func (e *Engine) portForward(ctx context.Context, pod, port string) (net.Conn, error) {
	rt, upgrader, err := spdy.RoundTripperFor(e.rest)
	if err != nil {
		return nil, err
	}
	u := e.kube.CoreV1().RESTClient().Post().Resource("pods").Namespace(e.opts.Namespace).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
	type result struct {
		c   httpstream.Connection
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, _, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
		ch <- result{c, err}
	}()
	var conn httpstream.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("port-forward to %s: %w", pod, r.err)
		}
		conn = r.c
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
	h := http.Header{}
	h.Set(corev1.StreamType, corev1.StreamTypeError)
	h.Set(corev1.PortHeader, port)
	h.Set(corev1.PortForwardRequestIDHeader, "0")
	errStream, err := conn.CreateStream(h)
	if err != nil {
		conn.Close()
		return nil, err
	}
	errStream.Close() // our half: the kubelet only writes to it
	h.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(h)
	if err != nil {
		conn.Close()
		return nil, err
	}
	pc := &pfConn{conn: conn, data: data, pod: pod}
	// The kubelet reports a failure to connect to the port on the error stream.
	go func() {
		if b, _ := io.ReadAll(errStream); len(b) > 0 {
			e.log.Debug("port-forward error", "pod", pod, "port", port, "err", string(b))
			pc.Close()
		}
	}()
	return pc, nil
}

// pfConn is a port-forward data stream as a net.Conn. SPDY streams have no
// deadlines of their own, so a deadline here closes the stream when it
// passes: enough for the bounded handshakes callers set them for, though
// unlike a socket's it cannot be recovered from.
type pfConn struct {
	conn httpstream.Connection
	data httpstream.Stream
	pod  string

	mu    sync.Mutex
	timer *time.Timer
	once  sync.Once
}

func (c *pfConn) Read(b []byte) (int, error)  { return c.data.Read(b) }
func (c *pfConn) Write(b []byte) (int, error) { return c.data.Write(b) }
func (c *pfConn) Close() error {
	c.once.Do(func() {
		c.data.Close()
		c.conn.Close()
	})
	return nil
}
func (c *pfConn) LocalAddr() net.Addr  { return pfAddr("sandpitd") }
func (c *pfConn) RemoteAddr() net.Addr { return pfAddr("pod/" + c.pod) }
func (c *pfConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if !t.IsZero() {
		c.timer = time.AfterFunc(time.Until(t), func() { c.Close() })
	}
	return nil
}
func (c *pfConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *pfConn) SetWriteDeadline(t time.Time) error { return c.SetDeadline(t) }

type pfAddr string

func (a pfAddr) Network() string { return "port-forward" }
func (a pfAddr) String() string  { return string(a) }

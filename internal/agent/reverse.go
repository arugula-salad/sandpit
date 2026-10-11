package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// ReverseHost is the guest's channel to sandpitd where there is no vsock (an
// Agent Substrate actor, reached only through its router): sandpitd opens
// streams to the agent (GET /internal/hostchan, an upgrade like the TCP
// proxy's), the agent parks them, and Dial hands one out each time the guest
// needs the host (the in-guest API, service reports). sandpitd serves its side
// of the guest API on each, and opens another whenever one is taken. The
// streams come in on the agent's authenticated listener, so whoever is at the
// other end is the sandpitd this sandbox belongs to.
type ReverseHost struct {
	idle chan net.Conn
}

func NewReverseHost() *ReverseHost { return &ReverseHost{idle: make(chan net.Conn, 16)} }

// reverseWait is how long Dial waits for sandpitd to have a stream parked: a
// sandbox restored a moment ago has none until sandpitd opens them.
const reverseWait = 10 * time.Second

// Dial takes a parked stream, waiting a while for one.
func (r *ReverseHost) Dial(ctx context.Context) (net.Conn, error) {
	t := time.NewTimer(reverseWait)
	defer t.Stop()
	for {
		select {
		case c := <-r.idle:
			return c, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			return nil, errors.New("no channel to sandpitd yet")
		}
	}
}

// handle parks one stream sandpitd opened.
func (r *ReverseHost) handle(w http.ResponseWriter, req *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "internal", "hijack unsupported")
		return
	}
	c, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	// sandpitd opens the first stream after each resume with reset: the ones
	// parked before the sandbox was suspended went with it.
	if req.URL.Query().Get("reset") == "1" {
		for drained := false; !drained; {
			select {
			case old := <-r.idle:
				old.Close()
			default:
				drained = true
			}
		}
	}
	io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sandpit-hostchan\r\n\r\n")
	if buf.Reader.Buffered() > 0 { // sandpitd says nothing until it is spoken to
		c.Close()
		return
	}
	select {
	case r.idle <- c:
	default: // plenty parked already
		c.Close()
	}
}

// Register adds the endpoint sandpitd opens the streams on.
func (r *ReverseHost) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /internal/hostchan", r.handle)
}

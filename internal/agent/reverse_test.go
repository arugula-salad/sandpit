package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// sandpitd's side parks streams; the guest's Dial takes them, and a reset
// drops the ones parked before it.
func TestReverseHost(t *testing.T) {
	r := NewReverseHost()
	mux := http.NewServeMux()
	r.Register(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, mux)

	park := func(reset bool) net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		q := ""
		if reset {
			q = "?reset=1"
		}
		fmt.Fprintf(c, "GET /internal/hostchan%s HTTP/1.1\r\nHost: agent\r\nConnection: Upgrade\r\nUpgrade: sandpit-hostchan\r\n\r\n", q)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("park: %v %v", resp, err)
		}
		return c
	}
	stale := park(false)
	fresh := park(true) // drops stale
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g, err := r.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// What the guest writes arrives on the stream sandpitd parked last.
	g.Write([]byte("ping\n"))
	fresh.SetReadDeadline(time.Now().Add(2 * time.Second))
	if line, err := bufio.NewReader(fresh).ReadString('\n'); err != nil || line != "ping\n" {
		t.Fatalf("on the fresh stream: %q %v", line, err)
	}
	stale.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := stale.Read(make([]byte, 1)); err == nil {
		t.Fatal("the stale stream was not dropped")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if _, err := r.Dial(ctx2); err == nil {
		t.Fatal("Dial returned a stream with none parked")
	}
}

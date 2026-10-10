package engine

import (
	"context"
	"net"
	"net/http"
)

// Guest is a running sandbox as a front end reaches it: a stream to its guest
// agent (sandpit-agent's HTTP API), which DialPort, AgentDial and
// AgentTransport build on. A Firecracker VM (*vmm.Machine) dials it over
// vsock; another backend may dial it however it reaches its sandboxes (a pod
// over TCP, say). Every front end asks a guest for nothing else, which is what
// lets one run on an engine other than this one.
type Guest interface {
	Dial(ctx context.Context) (net.Conn, error)
}

// AgentDial reaches g's guest agent.
func AgentDial(g Guest) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) { return g.Dial(ctx) }
}

// AgentTransport carries HTTP to g's guest agent, a fresh stream per request:
// there is nothing to keep alive across a suspend.
func AgentTransport(g Guest) *http.Transport {
	return &http.Transport{DisableKeepAlives: true, DialContext: AgentDial(g)}
}

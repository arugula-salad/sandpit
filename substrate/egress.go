package substrate

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/netpolicy"
	"github.com/arugula-salad/sandpit/internal/store"
)

// A sandbox's network policy is its actor's Substrate egress policy. Both
// pick the most specific pattern for a name (an exact host, then the longest
// wildcard), but Substrate's has allows only, so a policy that allows
// everything is no egress policy at all, an allowlist becomes one, and a deny
// inside something allowed ("*" and then a deny) can't be enforced here.
// Substrate's egress gateway carries HTTP and TLS: an allowed host is
// reachable over those, on any port, and nothing else is.

// SetNetworkPolicy applies rules (compiled as p) to the sandbox's actor and
// stores them.
func (e *Engine) SetNetworkPolicy(id string, rules []store.NetworkRule, p *netpolicy.Policy) error {
	defer e.lock(id)()
	if _, err := e.store.GetRecord(id); err != nil {
		return err
	}
	if err := e.applyEgress(id, p); err != nil {
		return err
	}
	_, err := e.store.UpdateRecord(id, func(r *store.Record) { r.NetworkRules = rules })
	return err
}

// RepublishNetworkPolicy has nothing to do: the policy lives in Substrate.
func (e *Engine) RepublishNetworkPolicy(string) {}

// reapplyEgress gives a new actor (a restore's, a clone's) the policy the
// record holds. The caller holds the sandbox's lock.
func (e *Engine) reapplyEgress(id string) error {
	rec, err := e.store.GetRecord(id)
	if err != nil || len(rec.NetworkRules) == 0 {
		return err
	}
	p, err := netpolicy.Compile(rec.NetworkRules)
	if err != nil {
		return err
	}
	return e.applyEgress(id, p)
}

func (e *Engine) applyEgress(id string, p *netpolicy.Policy) error {
	patterns, unrestricted, ok := p.Allowlist()
	if !ok {
		return fmt.Errorf("%w: Agent Substrate's egress policies allow only, so a deny inside something allowed (\"*\" and then a deny) can't be enforced; list what to allow instead", engine.ErrUnenforceable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if unrestricted {
		if _, err := e.api.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{Actor: e.ref(id)}); err != nil && status.Code(err) != codes.NotFound {
			return fmt.Errorf("substrate: DeleteActorEgressPolicy: %w", err)
		}
		return nil
	}
	all := &ateapipb.Ports{All: &ateapipb.AllPorts{}}
	pol := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: e.opts.Atespace, Name: "default"},
		Rules: []*ateapipb.EgressRule{
			{Http: &ateapipb.HTTPRule{Hostnames: patterns, Ports: all}},
			{TlsPassthrough: &ateapipb.TLSPassthroughRule{Hostnames: patterns, Ports: all}},
		},
	}
	if len(patterns) == 0 { // allows nothing at all
		pol.Rules = nil
	}
	_, err := e.api.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{Actor: e.ref(id), EgressPolicy: pol})
	if status.Code(err) == codes.NotFound {
		_, err = e.api.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{Actor: e.ref(id), EgressPolicy: pol})
	}
	if err != nil {
		return fmt.Errorf("substrate: egress policy: %w", err)
	}
	return nil
}

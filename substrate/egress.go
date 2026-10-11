package substrate

import (
	"fmt"

	"github.com/arugula-salad/sandpit/engine"
	"github.com/arugula-salad/sandpit/internal/netpolicy"
	"github.com/arugula-salad/sandpit/internal/store"
)

// A sandbox reaches the network through its worker pod (home-cloud runs
// Substrate without its egress gateway, which carries HTTP(S) only and can't
// say "allow everything"), confined to the internet by a NetworkPolicy on the
// workers, which is the same for every sandbox. So a policy that restricts
// nothing is stored and is what every sandbox has, and one that restricts
// anything can't be enforced here.

// SetNetworkPolicy stores an unrestricted policy and refuses a restrictive one.
func (e *Engine) SetNetworkPolicy(id string, rules []store.NetworkRule, p *netpolicy.Policy) error {
	defer e.lock(id)()
	if p.Restrictive() {
		return fmt.Errorf("%w: on Agent Substrate a sandbox's network is its worker's, the same for every sandbox (the internet, nothing inside the cluster); per-sandbox allowlists aren't enforced", engine.ErrUnenforceable)
	}
	_, err := e.store.UpdateRecord(id, func(r *store.Record) { r.NetworkRules = rules })
	return err
}

// RepublishNetworkPolicy has nothing to republish.
func (e *Engine) RepublishNetworkPolicy(string) {}

package netpolicy

import (
	"slices"
	"testing"

	"github.com/arugula-salad/sandpit/internal/store"
)

func TestAllowlist(t *testing.T) {
	r := func(d, a string) store.NetworkRule { return store.NetworkRule{Domain: d, Action: a} }
	for name, c := range map[string]struct {
		rules        []store.NetworkRule
		patterns     []string
		unrestricted bool
		ok           bool
	}{
		"none":            {nil, nil, true, true},
		"allow all":       {[]store.NetworkRule{r("*", "allow")}, nil, true, true},
		"allowlist":       {[]store.NetworkRule{r("pypi.org", "allow"), r("*.github.com", "allow"), r("*", "deny")}, []string{"*.github.com", "pypi.org"}, false, true},
		"redundant deny":  {[]store.NetworkRule{r("pypi.org", "allow"), r("evil.com", "deny")}, []string{"pypi.org"}, false, true},
		"deny under *":    {[]store.NetworkRule{r("*", "allow"), r("evil.com", "deny")}, nil, false, false},
		"deny under wild": {[]store.NetworkRule{r("*.example.com", "allow"), r("bad.example.com", "deny")}, nil, false, false},
	} {
		p, err := Compile(c.rules)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, unres, ok := p.Allowlist()
		if !slices.Equal(got, c.patterns) || unres != c.unrestricted || ok != c.ok {
			t.Errorf("%s: got %v %v %v, want %v %v %v", name, got, unres, ok, c.patterns, c.unrestricted, c.ok)
		}
	}
}

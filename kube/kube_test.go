package kube

import (
	"fmt"
	"testing"

	"github.com/arugula-salad/sandpit/frontend/e2b"
	"github.com/arugula-salad/sandpit/internal/store"
)

// The E2B front end runs on this engine.
var _ e2b.Engine = (*Engine)(nil)

func TestSandboxObject(t *testing.T) {
	e := &Engine{opts: Options{Namespace: "ns", Image: "img", AgentPort: 2024, PersistPath: "/home/user", DiskSize: "1Gi", RuntimeClass: "gvisor"}}
	obj := e.sandboxObject(store.Record{ID: "iabc", API: "e2b", Hostname: "e2b", Config: store.Config{CPUs: 2, RamMB: 512}})
	if obj.GetName() != "sandpit-iabc" || obj.GetNamespace() != "ns" {
		t.Fatalf("name %s/%s", obj.GetNamespace(), obj.GetName())
	}
	spec := obj.Object["spec"].(map[string]any)
	if spec["operatingMode"] != "Running" {
		t.Errorf("operatingMode = %v", spec["operatingMode"])
	}
	pod := spec["podTemplate"].(map[string]any)["spec"].(map[string]any)
	if pod["runtimeClassName"] != "gvisor" || pod["hostname"] != "e2b" || pod["automountServiceAccountToken"] != false {
		t.Errorf("pod spec: %v", pod)
	}
	c := pod["containers"].([]any)[0].(map[string]any)
	lim := c["resources"].(map[string]any)["limits"].(map[string]any)
	if lim["cpu"] != "2" || lim["memory"] != "512Mi" {
		t.Errorf("limits: %v", lim)
	}
	if len(spec["volumeClaimTemplates"].([]any)) != 1 || len(pod["initContainers"].([]any)) != 1 {
		t.Fatalf("no volume or no seeding: %v", spec)
	}
	// Every container sized alike, so the pod is Guaranteed (resourcesOf).
	seed := pod["initContainers"].([]any)[0].(map[string]any)
	if fmt.Sprint(seed["resources"]) != fmt.Sprint(c["resources"]) {
		t.Errorf("seed resources %v, sandbox %v", seed["resources"], c["resources"])
	}
}

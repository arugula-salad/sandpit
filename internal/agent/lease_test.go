package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The notice sandpitd sends ahead of a lease's deadline holds the sprite
// awake, says when it ends, and runs the guest's hooks with the deadline.
func TestLeaseExpiringNotice(t *testing.T) {
	state := t.TempDir()
	hooks := filepath.Join(state, "hooks", "lease-expiring.d")
	os.MkdirAll(hooks, 0o755)
	out := filepath.Join(state, "hook.out")
	os.WriteFile(filepath.Join(hooks, "10-push"), []byte("#!/bin/sh\necho \"$SPRITE_LEASE_EXPIRES_AT\" > "+out+"\n"), 0o755)
	os.WriteFile(filepath.Join(hooks, "README"), []byte("not executable\n"), 0o644)
	s := &Server{Sessions: NewManager(), StateDir: state}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	at := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	resp, err := http.Post(ts.URL+"/internal/lease-expiring", "application/json",
		strings.NewReader(`{"expires_at":"`+at.Format(time.RFC3339)+`"}`))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("notice: %v %v", resp, err)
	}
	if task, ok := s.tasks.get(leaseTask); !ok || task.ExpiresAt.Before(at.Add(-2*time.Second)) || task.ExpiresAt.After(at.Add(2*time.Second)) {
		t.Fatalf("hold task = %+v %v, want one ending at %s", task, ok, at)
	}
	var lease struct {
		ExpiresAt string `json:"expires_at"`
	}
	b, _ := os.ReadFile(filepath.Join(state, "lease.json"))
	if json.Unmarshal(b, &lease) != nil || lease.ExpiresAt != at.Format(time.RFC3339) {
		t.Fatalf("lease.json = %s", b)
	}
	var got []byte
	waitFor(t, "the hook to run", func() bool {
		got, _ = os.ReadFile(out)
		return bytes.HasSuffix(got, []byte("\n"))
	})
	if strings.TrimSpace(string(got)) != at.Format(time.RFC3339) {
		t.Fatalf("hook saw %q", got)
	}

	if resp, _ := http.Post(ts.URL+"/internal/lease-expiring", "application/json", strings.NewReader(`{}`)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("notice without expires_at: %d", resp.StatusCode)
	}
}

// A keep_awake service holds the sprite awake while it runs; any other does not.
func TestKeepAwakeServiceIsActivity(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	activity := func() map[string]any {
		resp, err := http.Get(ts.URL + "/internal/activity")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var a map[string]any
		json.NewDecoder(resp.Body).Decode(&a)
		return a
	}
	put(t, ts, "plain", `{"cmd":"sleep","args":["30"]}`, "duration=10ms")
	if a := activity(); a["awake_services"] != 0.0 {
		t.Fatalf("plain service: %v", a)
	}
	put(t, ts, "cron", `{"cmd":"sleep","args":["30"],"keep_awake":true}`, "duration=10ms")
	time.Sleep(50 * time.Millisecond)
	if a := activity(); a["awake_services"] != 1.0 || a["idle_ms"].(float64) > 40 {
		t.Fatalf("keep_awake service: %v", a)
	}
	if svc, _ := sv.Get("cron"); !svc.KeepAwake {
		t.Fatalf("definition lost keep_awake: %+v", svc.ServiceDef)
	}
	sv.Stop("cron", time.Second)
	if a := activity(); a["awake_services"] != 0.0 {
		t.Fatalf("stopped keep_awake service: %v", a)
	}
}

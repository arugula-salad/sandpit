package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newServiceServer(t *testing.T) (*httptest.Server, *Supervisor, string) {
	dir := t.TempDir()
	sv := NewSupervisor(dir+"/state", dir+"/run")
	ts := httptest.NewServer((&Server{Sessions: NewManager(), Services: sv}).Handler())
	t.Cleanup(func() {
		ts.Close()
		for _, s := range sv.List() {
			sv.Stop(s.Name, time.Second)
		}
	})
	return ts, sv, dir
}

// put creates a service and returns the streamed events.
func put(t *testing.T, ts *httptest.Server, name, body, query string) (int, []ServiceEvent) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/services/"+name+"?"+query, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []ServiceEvent
	if resp.StatusCode == http.StatusOK {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var ev ServiceEvent
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
			}
			evs = append(evs, ev)
		}
	}
	return resp.StatusCode, evs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServiceCreateStreamsLogsAndWritesLogFile(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	code, evs := put(t, ts, "web", `{"cmd":"sh","args":["-c","echo hello; echo oops >&2; sleep 30"],"env":{"A":"b"}}`, "duration=400ms")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var types []string
	for _, e := range evs {
		types = append(types, e.Type+":"+e.Data)
	}
	got := strings.Join(types, " ")
	for _, want := range []string{"started:pid", "stdout:hello", "stderr:oops", "complete:"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream %q missing %q", got, want)
		}
	}
	if last := evs[len(evs)-1]; last.Type != "complete" || last.LogFiles["combined"] == "" {
		t.Errorf("last event = %+v", last)
	}
	svc, _ := sv.Get("web")
	if svc.State.Status != "running" || svc.State.PID == 0 || svc.State.StartedAt == nil {
		t.Fatalf("state = %+v", svc.State)
	}
	log, _ := os.ReadFile(sv.LogPath("web"))
	if !bytes.Contains(log, []byte("[stdout] hello")) || !bytes.Contains(log, []byte("[stderr] oops")) {
		t.Fatalf("log file = %q", log)
	}
}

func TestServiceCrashRestartsAndStopIsSticky(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	put(t, ts, "flaky", `{"cmd":"sleep","args":["30"]}`, "duration=50ms")
	first, _ := sv.Get("flaky")

	// Killing it behind the supervisor's back counts as a crash.
	resp, _ := http.Post(ts.URL+"/services/signal", "application/json", strings.NewReader(`{"name":"flaky","signal":"KILL"}`))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("signal status %d", resp.StatusCode)
	}
	waitFor(t, "restart after crash", func() bool {
		s, _ := sv.Get("flaky")
		return s.State.Status == "running" && s.State.PID != 0 && s.State.PID != first.State.PID && s.State.RestartCount == 1
	})

	resp, _ = http.Post(ts.URL+"/services/flaky/stop?timeout=2s", "", nil)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"stopped"`) || !strings.Contains(string(body), `"type":"complete"`) {
		t.Fatalf("stop stream: %s", body)
	}
	time.Sleep(1500 * time.Millisecond) // longer than the restart backoff
	if s, _ := sv.Get("flaky"); s.State.Status != "stopped" || s.State.PID != 0 {
		t.Fatalf("stopped service came back: %+v", s.State)
	}
	resp, _ = http.Post(ts.URL+"/services/signal", "application/json", strings.NewReader(`{"name":"flaky","signal":"TERM"}`))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("signal to stopped service: status %d, want 409", resp.StatusCode)
	}
}

func TestServiceMissingBinaryIsReportedNotFatal(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	code, evs := put(t, ts, "ghost", `{"cmd":"no-such-binary-anywhere"}`, "duration=50ms")
	if code != http.StatusOK || evs[0].Type != "error" {
		t.Fatalf("code=%d events=%+v", code, evs)
	}
	if s, _ := sv.Get("ghost"); s.State.Status != "failed" || s.State.NextRestartAt == nil {
		t.Fatalf("state = %+v", s.State)
	}
}

func TestServiceValidation(t *testing.T) {
	ts, _, _ := newServiceServer(t)
	if code, _ := put(t, ts, "c", `{"cmd":"sleep","needs":["nope"]}`, ""); code != http.StatusBadRequest {
		t.Errorf("unknown dependency: status %d, want 400", code)
	}
	if resp, _ := http.Get(ts.URL + "/services/missing"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get missing: status %d", resp.StatusCode)
	}
}

func TestServicesStartInDependencyOrderOnBoot(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir+"/state/services", 0o755)
	// Names sort opposite to the dependency order, so alphabetical startup would fail this.
	for name, needs := range map[string]string{"a-app": `["m-cache"]`, "m-cache": `["z-db"]`, "z-db": `[]`} {
		def := fmt.Sprintf(`{"name":%q,"cmd":"sleep","args":["30"],"needs":%s}`, name, needs)
		os.WriteFile(dir+"/state/services/"+name+".json", []byte(def), 0o644)
	}
	sv := NewSupervisor(dir+"/state", dir+"/run") // what happens at cold boot
	t.Cleanup(func() {
		for _, s := range sv.List() {
			sv.Stop(s.Name, time.Second)
		}
	})
	// Spawning is synchronous inside NewSupervisor, so the recorded start times
	// are the order. Asserting on those (not on anything the children do) keeps
	// this independent of how loaded the machine is.
	started := map[string]time.Time{}
	for _, s := range sv.List() {
		if s.State.Status != "running" || s.State.StartedAt == nil {
			t.Fatalf("%s not running after boot: %+v", s.Name, s.State)
		}
		started[s.Name] = *s.State.StartedAt
	}
	if !(started["z-db"].Before(started["m-cache"]) && started["m-cache"].Before(started["a-app"])) {
		t.Fatalf("start order wrong: %v", started)
	}
}

// One service holds the HTTP port: a second is refused while the first has it.
// The listing gives http_port as null, not absent, for a service without one,
// so a caller comparing it with what it wants sees the port go as well as come.
func TestServiceHTTPPortConflictAndNullListing(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	put(t, ts, "a", `{"cmd":"sleep","args":["30"],"http_port":3000}`, "duration=10ms")
	if code, _ := put(t, ts, "b", `{"cmd":"sleep","args":["30"],"http_port":4000}`, ""); code != http.StatusConflict {
		t.Fatalf("second http_port service: status %d, want 409", code)
	}
	put(t, ts, "c", `{"cmd":"sleep","args":["30"]}`, "duration=10ms")
	if def, _ := os.ReadFile(filepath.Join(sv.defsDir, "c.json")); !bytes.Contains(def, []byte(`"http_port": null`)) {
		t.Errorf("c's saved definition: %s", def)
	}
	resp, err := http.Get(ts.URL + "/services")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("list: %v: %s", err, body)
	}
	for _, s := range list {
		if _, ok := s["http_port"]; !ok {
			t.Errorf("listing of %v has no http_port key: %s", s["name"], body)
		}
	}
}

// Replacing a definition by delete and create works for a service another one
// needs: the dependant keeps running, and the need resolves again on create.
func TestServiceNeededByAnotherCanBeDeletedAndRecreated(t *testing.T) {
	ts, sv, _ := newServiceServer(t)
	put(t, ts, "app", `{"cmd":"sleep","args":["30"]}`, "duration=10ms")
	put(t, ts, "door", `{"cmd":"sleep","args":["30"],"needs":["app"]}`, "duration=10ms")
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/services/app", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("deleting a needed service: status %d", resp.StatusCode)
	}
	if door, _ := sv.Get("door"); door.State.Status != "running" {
		t.Fatalf("door after app's delete: %+v", door.State)
	}
	if code, _ := put(t, ts, "app", `{"cmd":"sleep","args":["31"]}`, "duration=10ms"); code != http.StatusOK {
		t.Fatalf("re-creating app: status %d", code)
	}
	// door's need resolves again: a restart starts app first, without error.
	resp, _ = http.Post(ts.URL+"/services/door/restart?duration=10ms", "", nil)
	resp.Body.Close()
	if app, _ := sv.Get("app"); app.State.Status != "running" || len(app.Args) != 1 || app.Args[0] != "31" {
		t.Fatalf("app = %+v", app)
	}
}

// A service gets the sprite's environment, as an exec session does, with its
// own env over it.
func TestServiceGetsTheSpritesEnvironment(t *testing.T) {
	dir := t.TempDir()
	sv := NewSpriteSupervisor(dir+"/state", dir+"/run", nil, []string{"HUD_IDENTITY_PATH=/state/id", "WHO=sprite"})
	t.Cleanup(func() {
		for _, s := range sv.List() {
			sv.Stop(s.Name, time.Second)
		}
	})
	out := filepath.Join(dir, "env.out")
	if err := sv.Define(ServiceDef{Name: "envy", Cmd: "sh", Args: []string{"-c", `echo "$HUD_IDENTITY_PATH $WHO" > ` + out + `; sleep 30`},
		Env: map[string]string{"WHO": "service"}}); err != nil {
		t.Fatal(err)
	}
	if err := sv.Start("envy"); err != nil {
		t.Fatal(err)
	}
	var got []byte
	waitFor(t, "the service to write its environment", func() bool {
		got, _ = os.ReadFile(out)
		return bytes.HasSuffix(got, []byte("\n"))
	})
	if string(got) != "/state/id service\n" {
		t.Fatalf("service saw %q, want the sprite's HUD_IDENTITY_PATH and its own WHO", got)
	}
}

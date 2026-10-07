package agent

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// A sprite whose lease runs out is deleted, disk and all (engine/leases.go).
// sandpitd tells the guest first, a lead time ahead (the lease warning, 5
// minutes by default), on POST /internal/lease-expiring, waking the sprite if
// it has to. That lead time is the guest's grace period:
//
//   - a task named lease-expiring holds the sprite awake until the deadline
//     (an hour at most), so the warning is not slept through;
//   - /.sprite/lease.json says when it ends ({"expires_at": RFC 3339});
//   - each executable in /.sprite/hooks/lease-expiring.d runs, in name order,
//     as the sprite user, with SPRITE_LEASE_EXPIRES_AT set: the place to push
//     what would otherwise be lost.
//
// A renewal does not take any of it back; the task simply runs out.

const leaseTask = "lease-expiring"

func (s *Server) handleLeaseExpiring(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if json.Unmarshal(b, &body) != nil || body.ExpiresAt.IsZero() {
		writeErr(w, http.StatusBadRequest, "bad_request", "expires_at is required")
		return
	}
	hold := time.Until(body.ExpiresAt)
	if hold > maxTaskExpire {
		hold = maxTaskExpire
	}
	if hold > 0 {
		s.tasks.put(leaseTask, hold, false)
	}
	at := body.ExpiresAt.UTC().Format(time.RFC3339)
	out, _ := json.Marshal(map[string]string{"expires_at": at})
	os.WriteFile(filepath.Join(s.stateDir(), "lease.json"), append(out, '\n'), 0o644)
	go runLeaseHooks(filepath.Join(s.stateDir(), "hooks", "lease-expiring.d"), at)
	w.WriteHeader(http.StatusNoContent)
}

// runLeaseHooks runs the hooks one after another; each gets until the
// deadline. A hook's output goes to the agent's log.
func runLeaseHooks(dir, at string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	cred, home, uname := defaultUser()
	env := append(baseEnv(home, uname), "SPRITE_LEASE_EXPIRES_AT="+at)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
			continue
		}
		path := filepath.Join(dir, e.Name())
		cmd := &exec.Cmd{Path: path, Args: []string{path}, Env: env, Dir: home,
			SysProcAttr: &syscall.SysProcAttr{Credential: cred, Setpgid: true}}
		out, err := cmd.CombinedOutput()
		log.Printf("lease-expiring hook %s: err=%v output=%q", e.Name(), err, out)
	}
}

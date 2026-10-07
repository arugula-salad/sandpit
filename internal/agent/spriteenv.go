package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"time"
)

// FetchSpriteEnv asks sandpitd for the sprite's environment over the host
// channel, as KEY=value sorted by key, for NewSpriteSupervisor. A sandbox of
// another API, or a host that does not answer within timeout, gives none: the
// services start without it rather than not at all.
func FetchSpriteEnv(hostDial func(ctx context.Context) (net.Conn, error), timeout time.Duration) ([]string, error) {
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return hostDial(ctx) }}}
	resp, err := client.Get("http://host/internal/environment")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sprite environment: %s", resp.Status)
	}
	var body struct {
		Environment map[string]string `json:"environment"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("sprite environment: %w", err)
	}
	keys := make([]string, 0, len(body.Environment))
	for k := range body.Environment {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+body.Environment[k])
	}
	return env, nil
}

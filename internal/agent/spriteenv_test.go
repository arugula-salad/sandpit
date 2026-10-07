package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestFetchSpriteEnv(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   []string
	}{
		{200, `{"environment":{"WHO":"sprite","A":"1"}}`, []string{"A=1", "WHO=sprite"}},
		{404, `{"error":"not_found"}`, nil}, // another API's sandbox
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/internal/environment" {
				t.Errorf("path %s", r.URL.Path)
			}
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		dial := func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
		}
		got, err := FetchSpriteEnv(dial, time.Second)
		ts.Close()
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("status %d: env=%v err=%v, want %v", tc.status, got, err, tc.want)
		}
	}
}

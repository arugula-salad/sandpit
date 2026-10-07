package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The agent asks for the sprite's environment at boot, for its services.
func TestGuestChannelServesTheSpritesEnvironment(t *testing.T) {
	s, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"box","environment":{"HUD_IDENTITY_PATH":"/state/id"}}`), http.StatusCreated)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"bare"}`), http.StatusCreated)
	for name, want := range map[string]map[string]string{"box": {"HUD_IDENTITY_PATH": "/state/id"}, "bare": {}} {
		var got struct {
			Environment map[string]string `json:"environment"`
		}
		if err := json.Unmarshal(status(t, fromInside(t, s, name, "GET", "/internal/environment", ""), http.StatusOK), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Environment) != len(want) || got.Environment["HUD_IDENTITY_PATH"] != want["HUD_IDENTITY_PATH"] {
			t.Errorf("%s: environment = %v, want %v", name, got.Environment, want)
		}
	}
}

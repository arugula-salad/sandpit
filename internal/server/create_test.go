package server

import "testing"

// A name that is taken is a 409, as on sprites.dev: clients such as Fountain's
// Sprites SDK adopt the existing sprite on a 409 after a create whose reply was lost.
func TestCreateTakenNameIsConflict(t *testing.T) {
	_, h := newOperatorServer(t, Options{})
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"dev"}`), 201)
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"dev"}`), 409)
	// The image path checks the name before it pulls anything.
	status(t, apiCall(t, h, "POST", "/v1/sprites", `{"name":"dev","from":{"image":"node:22"}}`), 409)
}

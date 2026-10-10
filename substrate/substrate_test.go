package substrate

import "github.com/arugula-salad/sandpit/frontend/e2b"

// The E2B front end runs on this engine.
var _ e2b.Engine = (*Engine)(nil)

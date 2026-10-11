package substrate

import (
	"encoding/json"
	"testing"

	"github.com/arugula-salad/sandpit/internal/store"
)

// A checkpoint taken before the first start holds the template's snapshot: a
// sandbox restored or forked onto it must be claimed and booted again, or
// sandpitd asks an unclaimed agent for its token and gets the stream closed.
func TestColdCheckpoints(t *testing.T) {
	x := ext{AgentToken: "tok"}.tagged("v1") // created, checkpointed, never started
	x.Started = true                         // then it ran
	x = x.tagged("v2")

	if x.atCheckpoint("v1").Started {
		t.Error("restored to a cold checkpoint, still started")
	}
	if !x.atCheckpoint("v2").Started {
		t.Error("restored to a warm checkpoint, not started")
	}
	if got := x.atCheckpoint("v1").tagged("auto-1").Cold; len(got) != 2 {
		t.Errorf("cold after a restore to v1 and an auto: %v, want v1 and auto-1", got)
	}

	var src store.Record
	setExt(&src, x)
	for cp, started := range map[string]bool{"v1": false, "v2": true} {
		var y ext
		if err := json.Unmarshal(fromCheckpoint(src, cp), &y); err != nil {
			t.Fatal(err)
		}
		if y.Started != started || y.AgentToken != "tok" || len(y.Cold) != 0 {
			t.Errorf("fork from %s: %+v, want started=%v with src's token and no cold list", cp, y, started)
		}
	}
}

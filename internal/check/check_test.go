package check

import (
	"context"
	"testing"

	"github.com/thyarles/lhc/internal/state"
)

type cfg struct{ Toggle }

// stub cancels the run from inside its Run when cancel is set, as Ctrl+C
// arriving during that check would.
type stub struct {
	name   string
	ran    *[]string
	cancel context.CancelFunc
}

func (s stub) Meta() Meta       { return Meta{Name: s.name, Title: s.name} }
func (s stub) Defaults() Config { return &cfg{On} }
func (s stub) Run(_ context.Context, _ *Env) *Section {
	*s.ran = append(*s.ran, s.name)
	if s.cancel != nil {
		s.cancel()
	}
	return NewSection(s.name, s.name)
}

func TestRunAllStopsAtAnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ran []string
	checks := []Check{stub{name: "a", ran: &ran}, stub{name: "b", ran: &ran, cancel: cancel}, stub{name: "c", ran: &ran}}
	RunAll(ctx, checks, nil, &state.Dir{Path: t.TempDir(), ReadOnly: true}, Env{})
	if len(ran) != 2 || ran[1] != "b" {
		t.Fatalf("ran %v, want a and b only", ran)
	}
}

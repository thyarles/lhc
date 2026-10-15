package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The check's own 10-minute deadline must not become every command's limit:
// on a RHEL 7 host `yum history list` waited 2m08s on an unreachable RHN
// under it, where 30 seconds was meant.
func TestBudgetIgnoresAPlainDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if got := budget(ctx); got != DefaultTimeout {
		t.Fatalf("budget under a plain deadline = %v, want %v", got, DefaultTimeout)
	}
}

func TestWithTimeoutRaisesTheBudget(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if got := budget(ctx); got != 90*time.Second {
		t.Fatalf("budget = %v, want 90s", got)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("WithTimeout set no deadline for the group")
	}
}

func TestCommandStopsAtItsBudget(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := New().Run(ctx, "sleep", "10")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %v, want about 300ms", took)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) || res.Code != -1 {
		t.Fatalf("result %+v, want a timeout", res)
	}
}

// SIGINT comes first, so a tool that cleans up on Ctrl+C gets to do it.
func TestTimeoutSendsSIGINTFirst(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := New().Shell(ctx, `trap 'echo released locks >&2; exit 3' INT; sleep 10`)
	if took := time.Since(start); took >= KillGrace {
		t.Fatalf("took %v: the SIGINT handler should have ended it before SIGKILL", took)
	}
	if !strings.Contains(res.Stderr, "released locks") {
		t.Fatalf("stderr %q: the SIGINT handler did not run", res.Stderr)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want a timeout", res.Err)
	}
}

func TestTimeoutEndsWithSIGKILL(t *testing.T) {
	if testing.Short() {
		t.Skip("waits KillGrace")
	}
	ctx, cancel := WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The ignored SIGINT is inherited by sleep: only SIGKILL ends either.
	res := New().Shell(ctx, `trap '' INT; sleep 30`)
	took := time.Since(start)
	if took < KillGrace || took > KillGrace+5*time.Second {
		t.Fatalf("took %v, want about 300ms + %v", took, KillGrace)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want a timeout", res.Err)
	}
}

package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

type countingGate struct {
	calls int
	err   error
}

func (g *countingGate) Wait(context.Context) error {
	g.calls++
	return g.err
}

// 関門は登録した経路の工程開始前だけで待ち、待ちの失敗は工程を始めずに返す。
func TestTimePhaseWaitsAtRegisteredPriorityGate(t *testing.T) {
	t.Parallel()
	preparer := &Preparer{}
	ran := 0
	run := func() error { ran++; return nil }
	if err := preparer.timePhase(context.Background(), "checkout", run); err != nil || ran != 1 {
		t.Fatalf("unregistered phase err=%v ran=%d, want it to run without a gate", err, ran)
	}
	gate := &countingGate{}
	ctx := WithPriorityGate(context.Background(), gate)
	if err := preparer.timePhase(ctx, "checkout", run); err != nil || ran != 2 || gate.calls != 1 {
		t.Fatalf("gated phase err=%v ran=%d calls=%d, want one wait before the phase", err, ran, gate.calls)
	}
	gate.err = context.Canceled
	if err := preparer.timePhase(ctx, "cow", run); !errors.Is(err, context.Canceled) || ran != 2 {
		t.Fatalf("canceled gate err=%v ran=%d, want the phase skipped", err, ran)
	}
}

// common directory の排他を保持している区間では、関門で止まらず進む。
func TestPriorityGateIsSkippedWhileHoldingCommonDirLock(t *testing.T) {
	t.Parallel()
	gate := &countingGate{}
	ctx := WithPriorityGate(context.Background(), gate)
	runner := &gitx.Runner{}
	if err := runner.WithCommonDirLock(ctx, t.TempDir(), func(locked context.Context) error {
		if !gitx.HoldsCommonDirLock(locked) {
			t.Fatal("locked context does not report the common directory lock")
		}
		return yieldToPriority(locked)
	}); err != nil {
		t.Fatal(err)
	}
	if gate.calls != 0 {
		t.Fatalf("gate calls under the common directory lock=%d, want 0", gate.calls)
	}
	if gitx.HoldsCommonDirLock(ctx) {
		t.Fatal("outer context reports a lock it never acquired")
	}
}

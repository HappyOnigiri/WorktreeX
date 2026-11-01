package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// processStartTime を差し替えるため t.Parallel() を付けない。
func TestAgentProcessActiveAt(t *testing.T) {
	released := time.Date(2026, 10, 31, 11, 17, 42, 275907000, time.UTC)
	releasedAt := state.FormatTime(released)
	alive := os.Getpid()
	startAt := func(started time.Time, ok bool) func(int) (time.Time, bool) {
		return func(int) (time.Time, bool) { return started, ok }
	}
	tests := map[string]struct {
		pid        int
		releasedAt string
		start      func(int) (time.Time, bool)
		want       bool
	}{
		"reused pid started after release": {alive, releasedAt, startAt(released.Add(time.Hour), true), false},
		"agent started before release":     {alive, releasedAt, startAt(released.Add(-time.Hour), true), true},
		"started at release":               {alive, releasedAt, startAt(released, true), true},
		"start time unavailable":           {alive, releasedAt, startAt(time.Time{}, false), true},
		"released_at empty":                {alive, "", startAt(released.Add(time.Hour), true), true},
		"released_at malformed":            {alive, "not-a-time", startAt(released.Add(time.Hour), true), true},
		"zero pid":                         {0, releasedAt, startAt(released.Add(-time.Hour), true), false},
		"nonexistent pid":                  {1 << 30, releasedAt, startAt(released.Add(-time.Hour), true), false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			previous := processStartTime
			processStartTime = tt.start
			t.Cleanup(func() { processStartTime = previous })
			if got := agentProcessActiveAt(tt.pid, tt.releasedAt); got != tt.want {
				t.Fatalf("agentProcessActiveAt(%d, %q)=%v, want %v", tt.pid, tt.releasedAt, got, tt.want)
			}
		})
	}
}

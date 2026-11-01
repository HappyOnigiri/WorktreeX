package daemon

import (
	"os"
	"testing"
	"time"
)

func TestOSProcessStartTimeReadsOwnProcess(t *testing.T) {
	t.Parallel()
	started, ok := osProcessStartTime(os.Getpid())
	if !ok {
		t.Fatal("start time of the current process was unavailable")
	}
	if age := time.Since(started); age < 0 || age > time.Hour {
		t.Fatalf("start time %v is implausible for a test process (age %v)", started, age)
	}
	if _, ok := osProcessStartTime(1 << 30); ok {
		t.Fatal("start time of a nonexistent PID was reported")
	}
}

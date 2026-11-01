package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingDetailKeepsFailureAndExplainsRetention(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "removed.log")
	cause, _ := jobFailureCause("prepare", message("diag.standby.job_prepare", "Detail", "job"), "PREPARE_FAILED", "hook failed", path)
	lead, _ := jobFailureLead(path)
	if !strings.Contains(cause, "hook failed") || !strings.Contains(cause, "may have been removed") || !strings.Contains(lead, "daemon log") {
		t.Fatalf("cause=%q lead=%q", cause, lead)
	}
	if err := os.WriteFile(path, []byte("diagnostic"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause, _ = jobFailureCause("prepare", message("diag.standby.job_prepare", "Detail", "job"), "PREPARE_FAILED", "hook failed", path)
	lead, _ = jobFailureLead(path)
	if !strings.Contains(cause, "command output in") || !strings.Contains(lead, "read "+path) {
		t.Fatalf("cause=%q lead=%q", cause, lead)
	}
}

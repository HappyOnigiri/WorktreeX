package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceIssueReportsChangesSummaryAndRecovery(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	now := time.Now()
	m := &Manager{log: slog.New(slog.NewTextHandler(&logs, nil)), ctx: context.Background(), now: func() time.Time { return now }}
	report := func(signature string) { m.maintenanceIssue("repository", signature, "failed", slog.LevelError) }
	report("gone")
	report("gone")
	if strings.Count(logs.String(), "msg=failed") != 1 {
		t.Fatal(logs.String())
	}
	now = now.Add(maintenanceLogInterval)
	report("gone")
	if !strings.Contains(logs.String(), "suppressed=1") {
		t.Fatal(logs.String())
	}
	report("permissions")
	if strings.Count(logs.String(), "msg=failed") != 3 {
		t.Fatal(logs.String())
	}
	m.resolveMaintenanceIssue("repository")
	report("permissions")
	if strings.Count(logs.String(), "msg=failed") != 4 || !strings.Contains(logs.String(), "maintenance issue resolved") {
		t.Fatal(logs.String())
	}
	for i := range maintenanceLogKeys + 1 {
		m.maintenanceIssue(fmt.Sprint(i), "gone", "failed", slog.LevelError)
	}
	if len(m.maintenanceLogs) > maintenanceLogKeys {
		t.Fatal("unbounded suppression state")
	}
}

func TestGCSummaryBoundsExamplesAndIgnoresFailureIDs(t *testing.T) {
	t.Parallel()
	result := GCResult{}
	for i := range 168 {
		result.Reasons = append(result.Reasons, GCReason{Target: fmt.Sprintf("snapshot session%d/repository", i), Status: "pending", Reason: "source repository is unavailable: missing (failure ABC234)"})
	}
	signature, groups := summarizeGC(result, fmt.Errorf("joined long error"))
	if len(groups) != 1 || groups[0].Count != 168 {
		t.Fatalf("groups=%+v", groups)
	}
	for i := range result.Reasons {
		result.Reasons[i].Reason = "source repository is unavailable: missing (failure ZYX765)"
	}
	next, _ := summarizeGC(result, fmt.Errorf("another joined error"))
	if signature != next {
		t.Fatal("failure ID changed signature")
	}
	result.Reasons[0].Reason = "source repository is unavailable: permission denied"
	changedCause, _ := summarizeGC(result, nil)
	if signature == changedCause {
		t.Fatal("changed cause was suppressed")
	}
	result.Reasons = append(result.Reasons, GCReason{Target: "repository-other", Status: "failed", Reason: "permission denied"})
	changed, _ := summarizeGC(result, nil)
	if signature == changed {
		t.Fatal("new failure was suppressed")
	}
	for i := range 5 {
		result.Reasons = append(result.Reasons, GCReason{Target: fmt.Sprint(i), Status: "failed", Reason: strings.Repeat("x", 1000)})
	}
	_, groups = summarizeGC(result, nil)
	if len(groups) != 3 {
		t.Fatalf("examples=%d", len(groups))
	}
	for _, group := range groups {
		if len(group.Reason) > 515 {
			t.Fatal("unbounded reason")
		}
	}
}

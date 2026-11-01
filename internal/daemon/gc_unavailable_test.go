package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/archive"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func TestGCUnavailableRepositoryPreservesSnapshotsWithoutGitFailureLogs(t *testing.T) {
	t.Parallel()
	f := newOrphanRefFixture(t)
	now := time.Now().UTC()
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	tree := gitOutput(t, f.repository, "rev-parse", "HEAD^{tree}")
	snapshots := map[string][]state.Snapshot{}
	for i := range 20 {
		session := fmt.Sprintf("snapshot%d", i)
		snapshot := state.Snapshot{
			ID: session, SessionID: session, RepositoryID: f.repositoryID,
			HeadRef: "refs/wx/recovery/" + session + "/head", HeadOID: head,
			WorktreeRef: "refs/wx/recovery/" + session + "/worktree", WorktreeOID: head, IndexTreeOID: tree,
			Status: "ARCHIVED", CreatedAt: state.FormatTime(now.Add(-2 * time.Hour)), ExpiresAt: state.FormatTime(now.Add(-time.Hour)),
		}
		f.saveSnapshot(t, snapshot)
		snapshots[session] = []state.Snapshot{snapshot}
	}
	details := filepath.Join(t.TempDir(), "details")
	f.manager.git.SetDetailDir(details)
	moved := f.repository + "-moved"
	if err := os.Rename(f.repository, moved); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := archive.Manager{Git: f.manager.git}
	for range 2 {
		progress := f.manager.expireWorkspaceSnapshots(ctx, snapshots, &a)
		if progress.Pending != 20 || progress.Completed != 0 || progress.Failed != 0 {
			t.Fatalf("progress=%+v", progress)
		}
		f.manager.artifactOwnershipReport(ctx)
	}
	if entries, err := os.ReadDir(details); err == nil && len(entries) != 0 || err != nil && !os.IsNotExist(err) {
		t.Fatalf("unexpected diagnostic files: %v %v", entries, err)
	}
	for session := range snapshots {
		remaining, err := f.store.Snapshots(ctx, session)
		if err != nil || len(remaining) != 1 {
			t.Fatalf("snapshot lost: %s %v", session, err)
		}
	}
	if err := os.Rename(moved, f.repository); err != nil {
		t.Fatal(err)
	}
	progress := f.manager.expireWorkspaceSnapshots(ctx, snapshots, &a)
	if progress.Completed != 20 || progress.Pending != 0 || progress.err() != nil {
		t.Fatalf("did not recover: %+v err=%v", progress, progress.err())
	}
}

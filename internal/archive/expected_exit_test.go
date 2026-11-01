package archive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryRefPublicationAndRepeatedDeletionDoNotLogMissingRefs(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	details := t.TempDir()
	manager.Git.SetDetailDir(details)
	snapshot, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "source", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot, nil); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := os.ReadDir(details); err != nil || len(entries) != 0 {
		for _, entry := range entries {
			data, _ := os.ReadFile(filepath.Join(details, entry.Name()))
			t.Log(string(data))
		}
		t.Fatalf("expected missing refs generated logs: %v %v", entries, err)
	}
}

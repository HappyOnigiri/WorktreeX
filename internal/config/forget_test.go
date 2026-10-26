package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceRemovalPreservesOtherSettingsAndRejectsExternalChanges(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "version: 2\nsystem:\n  future_setting: keep\nworkspaces:\n  ~/gone:\n    worktree: hot\n    future_setting: remove\n  ~/other:\n    worktree: cold\n    future_setting: keep\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	preview, found, err := PreviewWorkspaceRemoval("~/gone")
	if err != nil || !found {
		t.Fatalf("preview found=%v err=%v", found, err)
	}
	if err := os.WriteFile(path, []byte(original+"# external change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CommitEdit(preview); !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("commit error=%v", err)
	}
	preview, found, err = PreviewWorkspaceRemoval("~/gone")
	if err != nil || !found {
		t.Fatalf("preview found=%v err=%v", found, err)
	}
	if err := CommitEdit(preview); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "gone") || strings.Contains(string(data), "remove") {
		t.Fatalf("removed workspace restored: %s", data)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Workspaces) != 1 || raw.Workspaces["~/other"].Worktree != "cold" || len(raw.UnknownKeys()) != 2 {
		t.Fatalf("remaining config=%+v, unknown=%v", raw.Workspaces, raw.UnknownKeys())
	}
	_, found, err = PreviewWorkspaceRemoval(filepath.Join(t.TempDir(), "unknown"))
	if err != nil || found {
		t.Fatalf("unknown workspace found=%v err=%v", found, err)
	}
}

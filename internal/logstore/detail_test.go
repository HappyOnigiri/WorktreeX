package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetailPublishesBoundedOwnerOnlyLog(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "details")
	path := WriteDetail(dir, "failure", strings.Repeat("x", maxDetailBytes+1))
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxDetailBytes+30 || !strings.HasSuffix(string(data), "[output truncated]\n") {
		t.Fatalf("detail bytes=%d err=%v", len(data), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	for _, id := range []string{"", "../escape"} {
		if path := WriteDetail(dir, id, "test"); path != "" {
			t.Fatal(path)
		}
	}
	if path := WriteDetail("", "failure", "test"); path != "" {
		t.Fatal(path)
	}
}

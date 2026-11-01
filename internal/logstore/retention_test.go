package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRetentionLog(t *testing.T, dir, name string, size int, modified time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestCleanerRetainsNewestWithinCountBytesAndAge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		count int
		bytes int64
		want  int
	}{
		{"count", 2, 100, 2}, {"bytes", 10, 25, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now()
			writeRetentionLog(t, dir, "expired.log", 10, now.Add(-DetailMaxAge-time.Hour))
			for i, name := range []string{"a.log", "b.log", "c.log"} {
				writeRetentionLog(t, dir, name, 10, now.Add(time.Duration(i)*time.Second))
			}
			writeRetentionLog(t, dir, ".prepare-active.tmp", 10, now.Add(-DetailMaxAge-time.Hour))
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, "link.log")); err != nil {
				t.Fatal(err)
			}
			c, err := newCleaner(dir, now, tc.count, tc.bytes)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			steps := 0
			for {
				done, err := c.Step(1)
				if err != nil {
					t.Fatal(err)
				}
				steps++
				if len(c.kept) > tc.count || c.bytes > tc.bytes {
					t.Fatal("unbounded collector memory")
				}
				if done {
					break
				}
			}
			if steps < 6 {
				t.Fatalf("not incremental: %d steps", steps)
			}
			for _, name := range []string{"expired.log", "a.log"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("%s was not removed: %v", name, err)
				}
			}
			for _, name := range []string{"b.log", "c.log", ".prepare-active.tmp", "link.log"} {
				if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
				t.Fatalf("outside=%q %v", data, err)
			}
		})
	}
}

func TestCleanerUsesPinnedDirectoryAfterReplacement(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "details")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	writeRetentionLog(t, dir, "expired.log", 10, now.Add(-2*DetailMaxAge))
	c, err := newCleaner(dir, now, 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	moved := dir + "-old"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRetentionLog(t, dir, "expired.log", 10, now)
	for {
		done, err := c.Step(1)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if _, err := os.Stat(filepath.Join(moved, "expired.log")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "expired.log")); err != nil {
		t.Fatal(err)
	}
}

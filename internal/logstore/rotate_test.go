package logstore

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRotationBoundsGenerationsAcrossRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wxd.log")
	for round := 0; round < 2; round++ {
		w, err := openWriter(path, 64, 3)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if _, err := w.Write([]byte(strings.Repeat("x", 30) + "\n")); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 3 {
			t.Fatalf("entries=%v err=%v", entries, err)
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || info.Size() > 64 || info.Mode().Perm() != 0o600 {
				t.Fatalf("info=%v err=%v", info, err)
			}
		}
	}
}

func TestRotationHandlesLegacyOversizedFileAndRecord(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "wxd.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 128), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := openWriter(path, 64, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	data := bytes.Repeat([]byte("y"), 128)
	if n, err := w.Write(data); err != nil || n != len(data) {
		t.Fatalf("write=%d %v", n, err)
	}
	current, err := os.ReadFile(path)
	if err != nil || len(current) > 64 || !bytes.Contains(current, []byte("exceeded")) {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if _, err := w.Write([]byte(strings.Repeat("n", 20) + "\n")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() > 64 {
		t.Fatalf("legacy log retained: %v %v", info, err)
	}
}

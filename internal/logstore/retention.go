package logstore

import (
	"container/heap"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

const (
	DetailMaxFiles       = 10000
	DetailMaxBytes int64 = 100 << 20
	DetailMaxAge         = 7 * 24 * time.Hour
	CleanupBatch         = 1024
)

type retainedLog struct {
	name     string
	modified time.Time
	size     int64
}
type recentLogs []retainedLog

func (h recentLogs) Len() int { return len(h) }
func (h recentLogs) Less(i, j int) bool {
	if h[i].modified.Equal(h[j].modified) {
		return h[i].name < h[j].name
	}
	return h[i].modified.Before(h[j].modified)
}
func (h recentLogs) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *recentLogs) Push(v any)   { *h = append(*h, v.(retainedLog)) }
func (h *recentLogs) Pop() any     { old := *h; n := len(old) - 1; v := old[n]; *h = old[:n]; return v }

// Cleaner は既存の巨大な details も固定件数ずつ走査し、最新の上限件数だけをメモリに保持する。
// 完了まで上限超過は残り得る。走査中に追加されたログは次の走査で回収する。
// temporary・symlink・directory は触らず、全削除を pin した descriptor 内に閉じる。
type Cleaner struct {
	root      *os.Root
	directory *os.File
	kept      recentLogs
	bytes     int64
	cutoff    time.Time
	maxFiles  int
	maxBytes  int64
}

func NewCleaner(dir string) (*Cleaner, error) {
	return newCleaner(dir, time.Now(), DetailMaxFiles, DetailMaxBytes)
}

func newCleaner(dir string, now time.Time, maxFiles int, maxBytes int64) (*Cleaner, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return &Cleaner{root: root, directory: directory, cutoff: now.Add(-DetailMaxAge), maxFiles: maxFiles, maxBytes: maxBytes}, nil
}

// Step は最大 batch 件だけ読む。完了・失敗後の descriptor は呼び出し側が Close する。
func (c *Cleaner) Step(batch int) (bool, error) {
	if batch <= 0 {
		batch = CleanupBatch
	}
	entries, readErr := c.directory.ReadDir(batch)
	var result error
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".log") || !entry.Type().IsRegular() {
			continue
		}
		info, err := c.root.Lstat(entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(c.cutoff) {
			result = errors.Join(result, c.remove(entry.Name()))
			continue
		}
		heap.Push(&c.kept, retainedLog{entry.Name(), info.ModTime(), info.Size()})
		c.bytes += info.Size()
		for len(c.kept) > c.maxFiles || c.bytes > c.maxBytes {
			oldest := heap.Pop(&c.kept).(retainedLog)
			c.bytes -= oldest.size
			result = errors.Join(result, c.remove(oldest.name))
		}
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		result = errors.Join(result, readErr)
	}
	return readErr != nil, result
}

func (c *Cleaner) remove(name string) error {
	err := c.root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (c *Cleaner) Close() error { return errors.Join(c.directory.Close(), c.root.Close()) }

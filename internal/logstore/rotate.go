package logstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	DaemonMaxBytes    int64 = 10 << 20
	DaemonGenerations       = 5
)

// Writer はログの書込みと世代交代を直列化し、削除を pin 済みログディレクトリ内に閉じる。
type Writer struct {
	mu          sync.Mutex
	root        *os.Root
	file        *os.File
	name        string
	size        int64
	maxBytes    int64
	generations int
}

func Open(path string) (*Writer, error) { return openWriter(path, DaemonMaxBytes, DaemonGenerations) }

func openWriter(path string, maxBytes int64, generations int) (*Writer, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	w := &Writer{root: root, name: filepath.Base(path), maxBytes: maxBytes, generations: generations}
	for i := 1; i < generations; i++ {
		name := fmt.Sprintf("%s.%d", w.name, i)
		if info, err := root.Lstat(name); err == nil && info.Mode().IsRegular() && info.Size() > maxBytes {
			if err := root.Remove(name); err != nil {
				_ = root.Close()
				return nil, err
			}
		}
	}
	if err := w.reopen(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return w, nil
}

func (w *Writer) reopen() error {
	file, err := w.root.OpenFile(w.name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file, w.size = file, info.Size()
	return nil
}

func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	for i := w.generations - 1; i > 0; i-- {
		from := w.name
		if i > 1 {
			from = fmt.Sprintf("%s.%d", w.name, i-1)
		}
		// 旧版の巨大ログを世代へ残すと、世代数を制限しても使用量の上限を超える。
		if i == 1 && w.size > w.maxBytes {
			if err := w.root.Remove(from); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.Join(err, w.reopen())
			}
			continue
		}
		if err := w.root.Rename(from, fmt.Sprintf("%s.%d", w.name, i)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, w.reopen())
		}
	}
	return w.reopen()
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(data)
	if int64(len(data)) > w.maxBytes {
		data = []byte("{\"level\":\"WARN\",\"msg\":\"log record exceeded size limit\"}\n")
	}
	if w.file == nil {
		if err := w.reopen(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(data)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	return original, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	if w.file != nil {
		err = w.file.Close()
		w.file = nil
	}
	return errors.Join(err, w.root.Close())
}

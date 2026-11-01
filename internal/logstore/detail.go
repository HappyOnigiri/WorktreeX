package logstore

import (
	"os"
	"path/filepath"
)

const maxDetailBytes = 128 << 10

// WriteDetail は完成した診断だけを公開する。回収中の走査が書込み中のログを消す競合を避ける。
// 保存不能でも処理結果は変えず、空の path を返す。
func WriteDetail(dir, id, content string) string {
	if dir == "" || id == "" || filepath.Base(id) != id {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Close() }()
	_ = root.Chmod(".", 0o700)
	temporary := ".detail-" + id + ".tmp"
	file, err := root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ""
	}
	defer func() { _ = root.Remove(temporary) }()
	if len(content) > maxDetailBytes {
		content = content[:maxDetailBytes] + "\n[output truncated]\n"
	}
	_, writeErr := file.WriteString(content)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return ""
	}
	name := id + ".log"
	if err := root.Rename(temporary, name); err != nil {
		return ""
	}
	return filepath.Join(dir, name)
}

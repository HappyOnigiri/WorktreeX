package daemon

import (
	"errors"
	"os"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

// detailLogUnavailable は欠損だけを判定する。権限エラーを保持期限による削除と断定しない。
func detailLogUnavailable(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func detailLogCause(cause string, result i18n.Message, path string) (string, i18n.Message) {
	if detailLogUnavailable(path) {
		return cause + " (command output unavailable at " + path + "; it may have been removed by log retention)", message("diag.job.detail_unavailable", "Cause", result, "Path", path)
	}
	return cause + " (command output in " + path + ")", message("diag.job.failed_detail_path", "Cause", result, "Path", path)
}

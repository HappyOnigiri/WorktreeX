package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/logstore"
)

// maintainDetailLogs は業務用 GC と独立してログを回収する。GC の故障中もログを回収し続ける。
func maintainDetailLogs(ctx context.Context, directory string, logger *slog.Logger) {
	var cleaner *logstore.Cleaner
	defer func() {
		if cleaner != nil {
			_ = cleaner.Close()
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastError := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		var err error
		if cleaner == nil {
			cleaner, err = logstore.NewCleaner(directory)
		}
		done := true
		if cleaner != nil {
			done, err = cleaner.Step(logstore.CleanupBatch)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && time.Since(lastError) >= maintenanceLogInterval {
			logger.Warn("detail log cleanup failed", "error", err)
			lastError = time.Now()
		}
		delay := time.Second
		if done {
			if cleaner != nil {
				_ = cleaner.Close()
				cleaner = nil
			}
			delay = time.Minute
		}
		timer.Reset(delay)
	}
}

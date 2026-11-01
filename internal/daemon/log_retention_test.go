package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestDetailRetentionStopsWithoutWaitingForNextBatch(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "missing")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		maintainDetailLogs(ctx, directory, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not stop")
	}
}

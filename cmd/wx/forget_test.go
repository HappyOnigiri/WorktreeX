package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
)

type forgetTestHandler struct {
	mu      sync.Mutex
	path    string
	discard bool
	refuse  bool
	reloads int
}

func (h *forgetTestHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch method {
	case "Forget":
		var params struct {
			Path    string `json:"path"`
			Discard bool   `json:"discard_recovery"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		h.path, h.discard = params.Path, params.Discard
		if h.refuse {
			return nil, errors.New("workspace still has recovery state")
		}
		return daemon.ForgetResult{Root: params.Path}, nil
	case "ReloadConfig":
		h.reloads++
		return nil, nil
	default:
		return nil, errors.New("unexpected method: " + method)
	}
}

func TestRunForgetResolvesMissingPathAndRemovesSettingsOnlyAfterSuccess(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := "success"
		if refuse {
			name = "refused"
		}
		t.Run(name, func(t *testing.T) {
			home, err := os.MkdirTemp("/tmp", "wx-forget-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			home, err = filepath.EvalSymlinks(home)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", home)
			t.Chdir(home)
			cfg := config.DefaultsV2()
			cfg.Workspaces["~/gone"] = config.Workspace{Worktree: "hot"}
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
			handler := &forgetTestHandler{refuse: refuse}
			socket, err := config.SocketPath()
			if err != nil {
				t.Fatal(err)
			}
			cancel, done := serveUntilCanceled(t, socket, handler)
			t.Cleanup(func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			code := runForget(context.Background(), []string{"gone", "--discard-recovery"})
			if (code != 0) != refuse {
				t.Fatalf("exit=%d refuse=%v", code, refuse)
			}
			handler.mu.Lock()
			defer handler.mu.Unlock()
			if handler.path != filepath.Join(home, "gone") || !handler.discard {
				t.Fatalf("RPC path=%q discard=%v", handler.path, handler.discard)
			}
			raw, err := config.LoadRaw()
			if err != nil {
				t.Fatal(err)
			}
			_, configured := raw.Workspaces["~/gone"]
			if configured != refuse {
				t.Fatalf("configured=%v refuse=%v", configured, refuse)
			}
			if !refuse && handler.reloads != 1 {
				t.Fatalf("reloads=%d", handler.reloads)
			}
		})
	}
}

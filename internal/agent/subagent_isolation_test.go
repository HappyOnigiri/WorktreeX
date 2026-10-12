package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentPayload は Agent ツールの PreToolUse payload を組む。isolation が空なら指定なしの呼び出しになる。
func agentPayload(t *testing.T, toolName, isolation, prompt string, extra map[string]any) []byte {
	t.Helper()
	toolInput := map[string]any{"description": "Implement T-1", "subagent_type": "general-purpose", "prompt": prompt}
	if isolation != "" {
		toolInput["isolation"] = isolation
	}
	body := map[string]any{
		"session_id":      "test-session",
		"transcript_path": "/dev/null",
		"hook_event_name": "PreToolUse",
		"tool_name":       toolName,
		"tool_input":      toolInput,
		"cwd":             "/tmp/wx-session/root",
	}
	for key, value := range extra {
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSubagentIsolationHookOutputDeniesWorktreeIsolation(t *testing.T) {
	for name, payload := range map[string][]byte{
		"agent":        agentPayload(t, "Agent", "worktree", "Implement T-1.", nil),
		"former task":  agentPayload(t, "Task", "worktree", "Implement T-1.", nil),
		"extra fields": agentPayload(t, "Agent", "worktree", "Implement T-1.", map[string]any{"tool_use_id": "tool-1", "permission_mode": "bypassPermissions"}),
	} {
		t.Run(name, func(t *testing.T) {
			output, ok := subagentIsolationHookOutput(payload)
			if !ok {
				t.Fatal("worktree isolation was not denied")
			}
			specific := output.HookSpecificOutput
			if specific.PermissionDecision != "deny" || specific.HookEventName != "PreToolUse" || specific.UpdatedInput != nil {
				t.Fatalf("decision=%+v", specific)
			}
			// isolation を外すだけでは並列実行で衝突するため、代替手順まで理由に入れる。
			for _, want := range []string{"wx new", "absolute path", "prompt", "without isolation", "collide"} {
				if !strings.Contains(specific.PermissionDecisionReason, want) {
					t.Fatalf("reason %q does not mention %q", specific.PermissionDecisionReason, want)
				}
			}
			if output.SystemMessage != subagentIsolationSystemMessage {
				t.Fatalf("systemMessage=%q", output.SystemMessage)
			}
		})
	}
}

// 誤爆がこの判定の実害なので、通す呼び出しを厚く保つ。
func TestSubagentIsolationHookOutputPassesEverythingElse(t *testing.T) {
	passing := map[string][]byte{
		"no isolation": agentPayload(t, "Agent", "", "Implement T-1.", nil),
		// remote はローカルの worktree を作らない。
		"remote isolation": agentPayload(t, "Agent", "remote", "Implement T-1.", nil),
		// prompt 本文に isolation や worktree の語が入るのは普通なので、フィールドの値だけを見る。
		"prompt mentions isolation": agentPayload(t, "Agent", "", `Do not use isolation: "worktree"; run git worktree add --detach instead.`, nil),
		"bash":                      []byte(`{"tool_name":"Bash","tool_input":{"isolation":"worktree","command":"git status"}}`),
		"edit":                      []byte(`{"tool_name":"Edit","tool_input":{"isolation":"worktree"}}`),
		"write":                     []byte(`{"tool_name":"Write","tool_input":{"isolation":"worktree"}}`),
		"no isolation key":          []byte(`{"tool_name":"Bash","tool_input":{"command":"git status"}}`),
	}
	// 壊れた payload は fail-open にする。生の文字列での判定は正当な呼び出しを止め得る。
	for index, raw := range []string{
		"",
		"not json",
		"[]",
		`{"tool_name": "Agent", "tool_input": "isolation"}`,
		`{"tool_name": "Agent", "tool_input": {"isolation": null}}`,
		`{"tool_name": "Agent", "tool_input": {"isolation": ["worktree"]}}`,
		`{"tool_input": {"isolation": "worktree"}}`,
	} {
		passing["broken "+string(rune('a'+index))] = []byte(raw)
	}
	for name, payload := range passing {
		t.Run(name, func(t *testing.T) {
			if output, ok := subagentIsolationHookOutput(payload); ok {
				t.Fatalf("payload %s was decided: %+v", payload, output)
			}
		})
	}
}

// preToolUseDecision での deny は管理下 session に限る。wx 外の判定は保存済み方針を引く別経路が持つ。
func TestPreToolUseDecisionLimitsSubagentIsolationToManagedSessions(t *testing.T) {
	payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
	if _, ok := preToolUseDecision(context.Background(), payload, "/tmp/wx-session/root", true); !ok {
		t.Fatal("managed session did not deny worktree isolation")
	}
	if output, ok := preToolUseDecision(context.Background(), payload, "/tmp/wx-session/root", false); ok {
		t.Fatalf("a session outside wx management decided: %+v", output)
	}
}

func TestPreToolUseHookDeniesSubagentWorktreeIsolation(t *testing.T) {
	t.Run("managed session", func(t *testing.T) {
		clearHookEnvironment(t)
		handler := &recordingHandler{}
		ctx := startHookServer(t, handler)
		t.Setenv("WX_SESSION_ID", "wx-subagent")
		t.Setenv("WX_SESSION_TOKEN", "token")
		t.Setenv("WX_READINESS_TIMEOUT", "2s")
		t.Setenv("WX_WORKSPACE_ROOT", "/tmp/wx-session/root")
		payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
		output := captureHookStdout(t, func() {
			if err := RunHook(ctx, "pre-tool-use", strings.NewReader(string(payload))); err != nil {
				t.Fatal(err)
			}
		})
		// deny は daemon を必要としないので readiness を確かめずに出す。
		if methods := handler.methodsSnapshot(); len(methods) != 0 {
			t.Fatalf("methods=%v, want no RPC before the deny", methods)
		}
		var decoded preToolUseHookOutput
		if err := json.Unmarshal([]byte(output), &decoded); err != nil {
			t.Fatalf("stdout %q is not a single JSON object: %v", output, err)
		}
		if decoded.HookSpecificOutput.PermissionDecision != "deny" || strings.Contains(output, "updatedInput") {
			t.Fatalf("output=%s", output)
		}
	})
	t.Run("direct launch without a saved policy", func(t *testing.T) {
		clearHookEnvironment(t)
		t.Setenv("HOME", t.TempDir())
		t.Setenv("WX_DIRECT_ROOT", "/tmp/wx-session/root")
		payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
		output := captureHookStdout(t, func() {
			if err := RunHook(context.Background(), "pre-tool-use", strings.NewReader(string(payload))); err != nil {
				t.Fatal(err)
			}
		})
		if output != "" {
			t.Fatalf("direct launch emitted %q", output)
		}
	})
}

func TestUnmanagedSubagentIsolationFollowsSavedWorktreePolicy(t *testing.T) {
	payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", nil)
	for mode, deny := range map[string]bool{"hot": true, "cold": true, "off": false, "ask": false, "": false} {
		t.Run("mode "+mode, func(t *testing.T) {
			var asked string
			output, ok := unmanagedSubagentIsolationHookOutput(context.Background(), payload, func(_ context.Context, cwd string) (string, bool) {
				asked = cwd
				return mode, true
			})
			if asked != "/tmp/wx-session/root" {
				t.Fatalf("policy was looked up for %q, want the payload cwd", asked)
			}
			if ok != deny {
				t.Fatalf("decided=%v, want %v", ok, deny)
			}
			if !deny {
				return
			}
			specific := output.HookSpecificOutput
			if specific.PermissionDecision != "deny" || specific.UpdatedInput != nil {
				t.Fatalf("decision=%+v", specific)
			}
			// wx 外の session は終了しても lease を返さないため、id を控えて返す手順まで案内する。
			for _, want := range []string{"wx new --json", "without isolation", "collide", "wx release <session_id>"} {
				if !strings.Contains(specific.PermissionDecisionReason, want) {
					t.Fatalf("reason %q does not mention %q", specific.PermissionDecisionReason, want)
				}
			}
		})
	}
	t.Run("unresolved policy", func(t *testing.T) {
		if output, ok := unmanagedSubagentIsolationHookOutput(context.Background(), payload, func(context.Context, string) (string, bool) {
			return "hot", false
		}); ok {
			t.Fatalf("an unresolved policy was decided: %+v", output)
		}
	})
	t.Run("missing cwd falls back to the process directory", func(t *testing.T) {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		withoutCWD := []byte(`{"tool_name":"Agent","tool_input":{"isolation":"worktree","prompt":"Implement T-1."}}`)
		var asked string
		if _, ok := unmanagedSubagentIsolationHookOutput(context.Background(), withoutCWD, func(_ context.Context, cwd string) (string, bool) {
			asked = cwd
			return "hot", true
		}); !ok || asked != wd {
			t.Fatalf("decided=%v cwd=%q, want deny for %q", ok, asked, wd)
		}
	})
	// 全ツール呼び出しで走るため、対象外の呼び出しでは設定も Git も読まない。
	t.Run("other calls skip the lookup", func(t *testing.T) {
		for _, other := range [][]byte{
			agentPayload(t, "Agent", "", "Implement T-1.", nil),
			agentPayload(t, "Agent", "remote", "Implement T-1.", nil),
			[]byte(`{"tool_name":"Bash","tool_input":{"isolation":"worktree","command":"git status"}}`),
		} {
			if _, ok := unmanagedSubagentIsolationHookOutput(context.Background(), other, func(context.Context, string) (string, bool) {
				t.Fatalf("policy was looked up for %s", other)
				return "", false
			}); ok {
				t.Fatalf("payload %s was decided", other)
			}
		}
	})
}

// wx 外で起動した session でも、保存済み方針が hot / cold の workspace では deny し、off では通す。
func TestPreToolUseHookOutsideWxReadsSavedWorktreePolicy(t *testing.T) {
	clearHookEnvironment(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	hot := filepath.Join(tmp, "hot")
	off := filepath.Join(tmp, "off")
	for _, repository := range []string{hot, off} {
		if err := os.MkdirAll(filepath.Join(repository, "pkg"), 0o700); err != nil {
			t.Fatal(err)
		}
		fixtureGit(t, repository, "init", "-q", "-b", "main")
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "wx"), 0o700); err != nil {
		t.Fatal(err)
	}
	configYAML := "version: 2\nworkspaces:\n    " + hot + ":\n        worktree: hot\n    " + off + ":\n        worktree: \"off\"\n"
	if err := os.WriteFile(filepath.Join(home, ".config", "wx", "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for name, test := range map[string]struct {
		cwd  string
		deny bool
	}{
		"hot subdirectory": {cwd: filepath.Join(hot, "pkg"), deny: true},
		"off repository":   {cwd: off, deny: false},
		"outside":          {cwd: tmp, deny: false},
	} {
		t.Run(name, func(t *testing.T) {
			payload := agentPayload(t, "Agent", "worktree", "Implement T-1.", map[string]any{"cwd": test.cwd})
			output := captureHookStdout(t, func() {
				if err := RunHook(context.Background(), "pre-tool-use", strings.NewReader(string(payload))); err != nil {
					t.Fatal(err)
				}
			})
			if !test.deny {
				if output != "" {
					t.Fatalf("emitted %q", output)
				}
				return
			}
			var decoded preToolUseHookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("stdout %q is not a single JSON object: %v", output, err)
			}
			if decoded.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("output=%s", output)
			}
		})
	}
}

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// pendingReadinessHandler は準備が終わらない daemon を模す。
// WaitReady は期限まで返らず、LeaseProgress は state を返す。
type pendingReadinessHandler struct {
	mu      sync.Mutex
	state   string
	methods []string
}

func (h *pendingReadinessHandler) Handle(ctx context.Context, method string, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	h.methods = append(h.methods, method)
	h.mu.Unlock()
	switch method {
	case "WaitReady":
		<-ctx.Done()
		return nil, ctx.Err()
	case "LeaseProgress":
		return map[string]string{"state": h.state}, nil
	}
	return map[string]bool{"ok": true}, nil
}

func (h *pendingReadinessHandler) methodsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.methods...)
}

func setManagedHookSession(t *testing.T, id string) {
	t.Helper()
	t.Setenv("WX_SESSION_ID", id)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_WORKSPACE_ROOT", "/tmp/wx-session/root")
}

func TestReadinessBudgetIsCappedByTheCallerCeiling(t *testing.T) {
	for _, test := range []struct {
		env     string
		ceiling time.Duration
		want    time.Duration
	}{
		{env: "", ceiling: userPromptReadinessBudget, want: userPromptReadinessBudget},
		{env: "", ceiling: waitReadyBudget, want: waitReadyBudget},
		{env: "5s", ceiling: userPromptReadinessBudget, want: 5 * time.Second},
		{env: "10m", ceiling: waitReadyBudget, want: waitReadyBudget},
	} {
		t.Setenv("WX_READINESS_TIMEOUT", test.env)
		got, err := readinessBudget(test.ceiling)
		if err != nil || got != test.want {
			t.Fatalf("WX_READINESS_TIMEOUT=%q ceiling=%s: budget=(%s, %v), want %s", test.env, test.ceiling, got, err, test.want)
		}
	}
	// hook の 30 秒上限と Bash ツールの既定 2 分に収まることが値の根拠である。
	if userPromptReadinessBudget >= 30*time.Second || waitReadyBudget >= 2*time.Minute {
		t.Fatalf("budgets %s / %s exceed the agent limits", userPromptReadinessBudget, waitReadyBudget)
	}
}

// 期限内に準備が終わらない user-prompt-submit は失敗せず、待機を wx wait-ready へ引き継ぐ案内を出す。
func TestUserPromptSubmitHandsOverAPendingWorkspaceToWaitReady(t *testing.T) {
	clearHookEnvironment(t)
	ctx := startHookServer(t, &pendingReadinessHandler{state: "PREPARING"})
	setManagedHookSession(t, "wx-pending-prompt")
	t.Setenv("WX_READINESS_TIMEOUT", "200ms")
	output := captureHookStdout(t, func() {
		if err := RunHook(ctx, "user-prompt-submit", strings.NewReader("")); err != nil {
			t.Fatalf("a pending workspace failed the hook: %v", err)
		}
	})
	if output != userPromptPendingNotice+"\n" || !strings.Contains(output, "`wx wait-ready`") {
		t.Fatalf("stdout=%q, want the wx wait-ready notice", output)
	}
}

// daemon が先に期限切れを返しても、client 側の期限切れと同じく準備中として扱う。
func TestUserPromptSubmitTreatsADaemonDeadlineAsPending(t *testing.T) {
	clearHookEnvironment(t)
	ctx := startHookServer(t, &recordingHandler{err: context.DeadlineExceeded})
	setManagedHookSession(t, "wx-daemon-deadline")
	output := captureHookStdout(t, func() {
		if err := RunHook(ctx, "user-prompt-submit", strings.NewReader("")); err != nil {
			t.Fatalf("a daemon deadline failed the hook: %v", err)
		}
	})
	if output != userPromptPendingNotice+"\n" {
		t.Fatalf("stdout=%q, want the wx wait-ready notice", output)
	}
}

func TestUserPromptSubmitKeepsFailingOnOtherErrors(t *testing.T) {
	clearHookEnvironment(t)
	ctx := startHookServer(t, &recordingHandler{err: errors.New("workspace readiness failed: state=FAILED")})
	setManagedHookSession(t, "wx-prompt-failure")
	output := captureHookStdout(t, func() {
		if err := RunHook(ctx, "user-prompt-submit", strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "state=FAILED") {
			t.Fatalf("hook error=%v, want the readiness failure", err)
		}
	})
	if output != "" {
		t.Fatalf("a failed readiness emitted %q", output)
	}
}

func TestPreToolUseDeniesWhileTheWorkspaceIsPreparing(t *testing.T) {
	tests := []struct {
		name, command, want string
		rpc                 bool
	}{
		{name: "plain tool", command: "ls", want: "deny", rpc: true},
		// 書き換え対象でも準備中は wx new へ書き換えず、readiness の deny を出す。
		{name: "rewritable worktree add", command: "git worktree add --detach /tmp/x HEAD", want: "deny", rpc: true},
		{name: "wait-ready", command: "wx wait-ready", want: "", rpc: false},
		{name: "wait-ready with surrounding spaces", command: "  wx wait-ready \n", want: "", rpc: false},
		{name: "wait-ready combined", command: "wx wait-ready && ls", want: "deny", rpc: true},
		{name: "wait-ready with arguments", command: "wx wait-ready --help", want: "deny", rpc: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearHookEnvironment(t)
			handler := &pendingReadinessHandler{state: "PREPARING"}
			ctx := startHookServer(t, handler)
			setManagedHookSession(t, "wx-preparing-"+string(rune('a'+index)))
			payload, err := json.Marshal(map[string]any{"cwd": "/tmp/wx-session/root", "tool_name": "Bash", "tool_input": map[string]string{"command": test.command}})
			if err != nil {
				t.Fatal(err)
			}
			output := captureHookStdout(t, func() {
				if err := RunHook(ctx, "pre-tool-use", bytes.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
			})
			methods := handler.methodsSnapshot()
			if test.rpc && (len(methods) != 1 || methods[0] != "LeaseProgress") {
				t.Fatalf("methods=%v, want [LeaseProgress]", methods)
			}
			if !test.rpc && len(methods) != 0 {
				t.Fatalf("methods=%v, want no RPC for wx wait-ready", methods)
			}
			if test.want == "" {
				if output != "" {
					t.Fatalf("wx wait-ready was not passed through: %q", output)
				}
				return
			}
			var decoded preToolUseHookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("stdout %q is not a single JSON object: %v", output, err)
			}
			got := decoded.HookSpecificOutput
			if got.PermissionDecision != "deny" || got.UpdatedInput != nil || !strings.Contains(got.PermissionDecisionReason, "`wx wait-ready`") || !strings.Contains(got.PermissionDecisionReason, "state=PREPARING") {
				t.Fatalf("output=%s, want a readiness deny pointing at wx wait-ready", output)
			}
		})
	}
}

func TestPreToolUsePassesOnceTheLeaseIsFullyReady(t *testing.T) {
	for _, state := range []string{"READY", "LEASED"} {
		t.Run(state, func(t *testing.T) {
			clearHookEnvironment(t)
			ctx := startHookServer(t, &recordingHandler{state: state})
			setManagedHookSession(t, "wx-ready-"+state)
			output := captureHookStdout(t, func() {
				if err := RunHook(ctx, "pre-tool-use", strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)); err != nil {
					t.Fatal(err)
				}
			})
			if output != "" {
				t.Fatalf("state %s emitted %q", state, output)
			}
		})
	}
}

func TestWaitReadyPrintsReadinessAndThePrepareNotice(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{response: map[string]any{"ready": true, "notice": "wx prepare notice: failed"}}
	ctx := startHookServer(t, handler)
	setManagedHookSession(t, "wx-wait-ready")
	var stdout, stderr bytes.Buffer
	if err := RunWaitReady(ctx, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if want := waitReadyReadyMessage + "\nwx prepare notice: failed\n"; stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q, want %q", stdout.String(), stderr.String(), want)
	}
	var params map[string]any
	if err := json.Unmarshal(handler.paramsFor("WaitReady"), &params); err != nil {
		t.Fatal(err)
	}
	if params["notice"] != true || params["timeout_ms"] != float64(waitReadyBudget.Milliseconds()) {
		t.Fatalf("WaitReady params=%v, want notice and the wait-ready budget", params)
	}
}

func TestWaitReadyAsksToRunAgainWhenTheBudgetRunsOut(t *testing.T) {
	clearHookEnvironment(t)
	ctx := startHookServer(t, &pendingReadinessHandler{state: "PREPARING"})
	setManagedHookSession(t, "wx-wait-ready-pending")
	t.Setenv("WX_READINESS_TIMEOUT", "200ms")
	var stdout, stderr bytes.Buffer
	if err := RunWaitReady(ctx, &stdout, &stderr); !errors.Is(err, ErrReadinessPending) {
		t.Fatalf("error=%v, want ErrReadinessPending", err)
	}
	if stdout.Len() != 0 || stderr.String() != waitReadyPendingMessage+"\n" {
		t.Fatalf("stdout=%q stderr=%q, want the run-again message", stdout.String(), stderr.String())
	}
}

func TestWaitReadyRequiresAWXSession(t *testing.T) {
	clearHookEnvironment(t)
	var stdout, stderr bytes.Buffer
	if err := RunWaitReady(context.Background(), &stdout, &stderr); err == nil || !strings.Contains(err.Error(), "inside a wx session") {
		t.Fatalf("error=%v, want a missing session error", err)
	}
}

package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyWorktreeAddCommand(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		decision worktreeAddDecision
		rewrite  string
		reason   string
	}{
		{name: "detached HEAD", command: "git worktree add --detach /tmp/x HEAD", decision: worktreeAddRewrite, rewrite: "wx new"},
		{name: "no start point", command: "git worktree add ../wt", decision: worktreeAddRewrite, rewrite: "wx new"},
		{name: "branch start point", command: "git worktree add x feature", decision: worktreeAddRewrite, rewrite: "wx new --branch feature"},
		{name: "remote start point", command: "git worktree add x origin/feature/one", decision: worktreeAddRewrite, rewrite: "wx new --branch feature/one"},
		{name: "absolute git", command: "/usr/bin/git worktree add -f --no-checkout x", decision: worktreeAddRewrite, rewrite: "wx new"},
		{name: "quoted path", command: `git worktree add "/tmp/dir with space" main`, decision: worktreeAddRewrite, rewrite: "wx new --branch main"},
		{name: "compound", command: "git worktree add x && cd x", decision: worktreeAddDeny, reason: denyReasonCompound},
		{name: "redirect", command: "git worktree add x > log", decision: worktreeAddDeny, reason: denyReasonCompound},
		{name: "nested shell", command: `sh -c 'git worktree add x'`, decision: worktreeAddDeny, reason: denyReasonCompound},
		// cwd を持たない payload では相対の -C を解決できない。
		{name: "relative directory without cwd", command: "git -C other worktree add x", decision: worktreeAddDeny, reason: denyReasonDirectory},
		{name: "directory twice", command: "git -C a -C b worktree add x", decision: worktreeAddDeny, reason: denyReasonUnsupported},
		{name: "expanded directory", command: `git -C "$CS_ROOT/server" worktree add --detach "$WT" 3139d393c8470d47e7cf30810580acad50a907e7`, decision: worktreeAddDeny, reason: denyReasonExpandedDirectory},
		{name: "git dir", command: "git --git-dir=/other/.git worktree add x", decision: worktreeAddDeny, reason: denyReasonOtherRepository},
		{name: "environment assignment", command: "GIT_DIR=/other git worktree add x", decision: worktreeAddDeny, reason: denyReasonOtherRepository},
		{name: "branch creation", command: "git worktree add -b feature x", decision: worktreeAddDeny, reason: denyReasonBranchCreation},
		{name: "orphan", command: "git worktree add --orphan x", decision: worktreeAddDeny, reason: denyReasonBranchCreation},
		{name: "revision expression", command: "git worktree add x HEAD~1", decision: worktreeAddDeny, reason: denyReasonStartPoint},
		{name: "object name", command: "git worktree add --detach /abs/wt 3139d393c8470d47e7cf30810580acad50a907e7", decision: worktreeAddRewrite, rewrite: "wx new --commit 3139d393c8470d47e7cf30810580acad50a907e7"},
		{name: "short object name", command: "git worktree add -q --detach /tmp/wt 801e2e09", decision: worktreeAddRewrite, rewrite: "wx new --commit 801e2e09"},
		{name: "remote object name", command: "git worktree add x origin/1a2b3c4d5e", decision: worktreeAddDeny, reason: denyReasonStartPoint},
		{name: "repository selector", command: "git worktree add x repo=main", decision: worktreeAddDeny, reason: denyReasonStartPoint},
		{name: "unknown flag", command: "git worktree add --lock x", decision: worktreeAddDeny, reason: denyReasonUnsupported},
		// パスは捨てるので、展開があっても書き換える。
		{name: "expanded path", command: `git worktree add "$TMPDIR/x"`, decision: worktreeAddRewrite, rewrite: "wx new"},
		{name: "quoted variable path", command: `git worktree add --detach "$WT" origin/main`, decision: worktreeAddRewrite, rewrite: "wx new --branch main"},
		{name: "unquoted variable path", command: "git worktree add -q --detach $W origin/prod", decision: worktreeAddRewrite, rewrite: "wx new --branch prod"},
		{name: "command substitution path", command: `git worktree add --detach "$(mktemp -d)/wt" 801e2e09`, decision: worktreeAddRewrite, rewrite: "wx new --commit 801e2e09"},
		{name: "expanded start point", command: `git worktree add --detach /tmp/wt "$REV"`, decision: worktreeAddDeny, reason: denyReasonExpandedStartPoint},
		{name: "expanded flag", command: "git worktree add --$MODE /tmp/wt", decision: worktreeAddDeny, reason: denyReasonUnsupported},
		// 展開を含む連結は、展開ではなく連結を理由に拒否する。
		{name: "variable path in compound", command: `WT=/tmp/wt; git worktree add --detach "$WT" 3139d393c8470d47e7cf30810580acad50a907e7 && cd "$WT"`, decision: worktreeAddDeny, reason: denyReasonCompound},
		{name: "branch creation from remote", command: "git worktree add -b feature/x /abs/wt origin/feature/x", decision: worktreeAddDeny, reason: denyReasonBranchCreation},
		{name: "unterminated quote", command: `git worktree add 'x`, decision: worktreeAddDeny, reason: denyReasonUnsupported},
		{name: "missing path", command: "git worktree add", decision: worktreeAddDeny, reason: denyReasonUnsupported},
		{name: "extra positional", command: "git worktree add x main extra", decision: worktreeAddDeny, reason: denyReasonUnsupported},
		{name: "help", command: "git worktree add -h", decision: worktreeAddIgnore},
		{name: "other subcommand", command: "git worktree list", decision: worktreeAddIgnore},
		{name: "other git command", command: "git status --short", decision: worktreeAddIgnore},
		{name: "echo", command: `echo "git worktree add"`, decision: worktreeAddIgnore},
		{name: "grep", command: `grep -rn "worktree add" .`, decision: worktreeAddIgnore},
		{name: "make", command: "make test-focus PKG=./internal/workspace RUN=WorktreeAdd", decision: worktreeAddIgnore},
		{name: "heredoc body", command: "cat > note.md <<'EOF'\ngit worktree add x main\nEOF", decision: worktreeAddIgnore},
		{name: "heredoc body unquoted delimiter", command: "cat > s.sh <<EOF\n  git worktree add x\nEOF", decision: worktreeAddIgnore},
		{name: "heredoc body dash", command: "cat > s.sh <<-EOF\n\tgit worktree add x\n\tEOF", decision: worktreeAddIgnore},
		{name: "heredoc body nested shell", command: "cat > s.sh <<'EOF'\nsh -c 'git worktree add x'\nEOF", decision: worktreeAddIgnore},
		{name: "heredoc body apostrophe", command: "cat > note.md <<'EOF'\ndon't run git worktree add here\nEOF", decision: worktreeAddIgnore},
		// 本文の後ろの候補は素通しになる。終端語を同定しない代償で、誤 deny より軽いと判断した。
		{name: "worktree add after heredoc", command: "cat <<'EOF'\ntext\nEOF\ngit worktree add x", decision: worktreeAddIgnore},
		{name: "here string", command: "grep worktree <<< 'git worktree add x'", decision: worktreeAddIgnore},
		{name: "heredoc after candidate", command: "git worktree add x && cat <<'EOF'\ntext\nEOF", decision: worktreeAddDeny, reason: denyReasonCompound},
		{name: "quoted heredoc operator", command: `git worktree add x && echo "a << b"`, decision: worktreeAddDeny, reason: denyReasonCompound},
		{name: "assignment-only command", command: "WX_FLAG=value", decision: worktreeAddIgnore},
		{name: "git global option without subcommand", command: "git --quiet", decision: worktreeAddIgnore},
		{name: "git worktree without subcommand", command: "git worktree", decision: worktreeAddIgnore},
		{name: "nested shell without command", command: "sh -c", decision: worktreeAddIgnore},
		{name: "nested shell with unrelated arguments", command: "sh foo bar", decision: worktreeAddIgnore},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := classifyWorktreeAddCommand(test.command, worktreeAddLocation{})
			if verdict.decision != test.decision {
				t.Fatalf("decision=%d, want %d (reason=%q command=%q)", verdict.decision, test.decision, verdict.reason, verdict.command)
			}
			if verdict.command != test.rewrite {
				t.Fatalf("rewrite=%q, want %q", verdict.command, test.rewrite)
			}
			if verdict.reason != test.reason {
				t.Fatalf("reason=%q, want %q", verdict.reason, test.reason)
			}
		})
	}
}

func TestIsAssignmentWordBoundaries(t *testing.T) {
	tests := []struct {
		word string
		want bool
	}{
		{word: "", want: false},
		{word: "=value", want: false},
		{word: "NAME=", want: true},
		{word: "NAME=value", want: true},
	}
	for _, test := range tests {
		t.Run(test.word, func(t *testing.T) {
			if got := isAssignmentWord(test.word); got != test.want {
				t.Fatalf("isAssignmentWord(%q)=%v, want %v", test.word, got, test.want)
			}
		})
	}
}

// TestDenyGuidanceCoversEveryReason は deny の理由が形ごとに異なり、どれにも wx new の案内があることを確認する。
func TestDenyGuidanceCoversEveryReason(t *testing.T) {
	reasons := []string{
		denyReasonCompound, denyReasonOtherRepository, denyReasonDirectory, denyReasonExpandedDirectory,
		denyReasonBranchCreation, denyReasonStartPoint, denyReasonExpandedStartPoint, denyReasonUnsupported,
	}
	seen := map[string]bool{}
	for _, reason := range reasons {
		if seen[reason] {
			t.Fatalf("deny reason %q is shared by two forms", reason)
		}
		seen[reason] = true
		if !strings.Contains(denyGuidance[reason], "wx new") {
			t.Fatalf("guidance for %q=%q, want a wx new command", reason, denyGuidance[reason])
		}
	}
	if len(denyGuidance) != len(reasons) {
		t.Fatalf("denyGuidance has %d entries, want %d", len(denyGuidance), len(reasons))
	}
}

func TestLexShellCommandDoesNotEmitEmptySegments(t *testing.T) {
	for _, command := range []string{"", ";", "git status;"} {
		t.Run(command, func(t *testing.T) {
			segments, ok := lexShellCommand(command)
			if !ok {
				t.Fatalf("lexShellCommand(%q) reported malformed input", command)
			}
			for index, segment := range segments {
				if len(segment) == 0 {
					t.Fatalf("lexShellCommand(%q) emitted empty segment at index %d", command, index)
				}
			}
		})
	}
}

func TestCommandRunsInWorkspace(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "wx", "slot")
	tests := []struct {
		cwd, root string
		want      bool
	}{
		{cwd: "", root: root, want: true},
		{cwd: root, root: root, want: true},
		{cwd: filepath.Join(root, "repo"), root: root, want: true},
		{cwd: root + "-other", root: root, want: false},
		{cwd: root, root: "", want: false},
		{cwd: filepath.Join(string(filepath.Separator), "elsewhere"), root: root, want: false},
	}
	for _, test := range tests {
		if got := commandRunsInWorkspace(test.cwd, test.root); got != test.want {
			t.Fatalf("commandRunsInWorkspace(%q, %q)=%v, want %v", test.cwd, test.root, got, test.want)
		}
	}
}

// TestWorktreeAddHookOutputKeepsUnknownToolInputFields は updatedInput が tool_input 全体を返すことを見る。
// 部分 merge を前提にすると description や timeout のような未知フィールドが落ちる。
func TestWorktreeAddHookOutputKeepsUnknownToolInputFields(t *testing.T) {
	payload := []byte(`{"cwd":"/wx/slot","tool_name":"Bash","tool_input":{"command":"git worktree add x feature","description":"branch out","timeout":120}}`)
	output, ok := worktreeAddHookOutput(payload, "/wx/slot")
	if !ok {
		t.Fatal("payload was not rewritten")
	}
	specific := output.HookSpecificOutput
	if specific.PermissionDecision != "allow" || specific.HookEventName != "PreToolUse" {
		t.Fatalf("decision=%+v", specific)
	}
	var command string
	if err := json.Unmarshal(specific.UpdatedInput["command"], &command); err != nil {
		t.Fatal(err)
	}
	if command != "wx new --branch feature" {
		t.Fatalf("command=%q", command)
	}
	if string(specific.UpdatedInput["description"]) != `"branch out"` || string(specific.UpdatedInput["timeout"]) != "120" {
		t.Fatalf("updatedInput dropped fields: %v", specific.UpdatedInput)
	}
	if output.SystemMessage != rewriteSystemMessage {
		t.Fatalf("systemMessage=%q", output.SystemMessage)
	}
}

// TestWorktreeAddHookOutputResolvesDirectoryFromPayloadCWD は git -C を payload の cwd から解決することを確認する。
func TestWorktreeAddHookOutputResolvesDirectoryFromPayloadCWD(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(root, "server")
	if err := os.MkdirAll(filepath.Join(server, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"cwd": root, "tool_input": map[string]any{"command": "git -C server worktree add --detach /abs/wt 3139d39"}})
	if err != nil {
		t.Fatal(err)
	}
	output, ok := worktreeAddHookOutput(payload, root)
	if !ok || output.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("output=%+v ok=%v, want a rewrite", output.HookSpecificOutput, ok)
	}
	var command string
	if err := json.Unmarshal(output.HookSpecificOutput.UpdatedInput["command"], &command); err != nil {
		t.Fatal(err)
	}
	if want := "(cd '" + server + "' && wx new --commit 3139d39)"; command != want {
		t.Fatalf("command=%q, want %q", command, want)
	}
}

func TestWorktreeAddHookOutputIgnoresUnusablePayloads(t *testing.T) {
	for _, payload := range []string{
		"",
		"{",
		`{"tool_input":{}}`,
		`{"tool_input":{"command":"git status"}}`,
		`{"tool_input":{"command":["git","worktree","add","x"]}}`,
		`{"cwd":"/elsewhere","tool_input":{"command":"git worktree add x"}}`,
	} {
		if _, ok := worktreeAddHookOutput([]byte(payload), "/wx/slot"); ok {
			t.Fatalf("payload %q produced a decision", payload)
		}
	}
}

func TestWorktreeAddHookDeniesWithoutUpdatedInput(t *testing.T) {
	payload := []byte(`{"cwd":"/wx/slot","tool_input":{"command":"git worktree add -b feature x"}}`)
	output, ok := worktreeAddHookOutput(payload, "/wx/slot")
	if !ok {
		t.Fatal("branch creation was not denied")
	}
	if output.HookSpecificOutput.PermissionDecision != "deny" || output.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("deny output=%+v", output.HookSpecificOutput)
	}
	if output.HookSpecificOutput.PermissionDecisionReason != denyReasonBranchCreation+" "+denyGuidance[denyReasonBranchCreation] {
		t.Fatalf("deny reason=%q", output.HookSpecificOutput.PermissionDecisionReason)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "updatedInput") {
		t.Fatalf("deny output carries updatedInput: %s", encoded)
	}
}

// TestPreToolUseHookDecidesAfterReadiness は録画 payload の command だけを差し替えて hook を通す。
// 実機から録っていない payload を testdata の録画名で置かないため、fixture は読み込んで組み替える。
func TestPreToolUseHookDecidesAfterReadiness(t *testing.T) {
	tests := []struct {
		name, fixture, command, want string
	}{
		{name: "claude rewrite", fixture: "claude-2.1.258-pre-tool-use.json", command: "git worktree add --detach /tmp/x HEAD", want: "allow"},
		{name: "codex deny", fixture: "codex-0.151.0-pre-tool-use-exec.json", command: "git worktree add -b feature /tmp/x", want: "deny"},
		{name: "claude untouched", fixture: "claude-2.1.258-pre-tool-use.json", command: `grep -rn "git worktree add" .`, want: ""},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearHookEnvironment(t)
			handler := &recordingHandler{}
			ctx := startHookServer(t, handler)
			t.Setenv("WX_SESSION_TOKEN", "token")
			t.Setenv("WX_SESSION_ID", "wx-decision-"+string(rune('a'+index)))
			t.Setenv("WX_READINESS_TIMEOUT", "2s")
			t.Setenv("WX_WORKSPACE_ROOT", "/tmp/wx-session/root")
			payload := recordedPayloadWithCommand(t, test.fixture, test.command)
			output := captureHookStdout(t, func() {
				if err := RunHook(ctx, "pre-tool-use", strings.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
			})
			// 書き換えと素通しは readiness を確かめ、daemon を必要としない deny は確かめずに出す。
			methods := handler.methodsSnapshot()
			if test.want == "deny" {
				if len(methods) != 0 {
					t.Fatalf("methods=%v, want no RPC before a deny", methods)
				}
			} else if len(methods) != 1 || methods[0] != "LeaseProgress" {
				t.Fatalf("methods=%v, want [LeaseProgress]", methods)
			}
			if test.want == "" {
				if output != "" {
					t.Fatalf("untouched command emitted %q", output)
				}
				return
			}
			var decoded preToolUseHookOutput
			if err := json.Unmarshal([]byte(output), &decoded); err != nil {
				t.Fatalf("stdout %q is not a single JSON object: %v", output, err)
			}
			if decoded.HookSpecificOutput.PermissionDecision != test.want {
				t.Fatalf("permissionDecision=%q, want %q", decoded.HookSpecificOutput.PermissionDecision, test.want)
			}
		})
	}
}

// recordedPayloadWithCommand は録画 payload の tool_input.command だけを差し替える。
func recordedPayloadWithCommand(t *testing.T, fixture, command string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	var recorded map[string]any
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatal(err)
	}
	toolInput, ok := recorded["tool_input"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no tool_input object", fixture)
	}
	toolInput["command"] = command
	rebuilt, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	return string(rebuilt)
}

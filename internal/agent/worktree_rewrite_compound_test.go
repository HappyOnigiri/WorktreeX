package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compoundFixture は server と web の 2 repository を持つ workspace root と、その外の repository を作る。
func compoundFixture(t *testing.T) (root, outside string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err = filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{filepath.Join(root, "server", ".git"), filepath.Join(root, "web", ".git"), filepath.Join(outside, "repo", ".git")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// TestClassifyWorktreeAddCommandRewritesCompound は、パスを変数で後続へ渡す連結だけを
// `NAME=$(wx new ...)` へ書き換え、残りの文字列をそのまま残すことを確認する。
func TestClassifyWorktreeAddCommandRewritesCompound(t *testing.T) {
	root, _ := compoundFixture(t)
	server := filepath.Join(root, "server")
	oid := "3139d393c8470d47e7cf30810580acad50a907e7"
	tests := []struct {
		name, cwd, command, rewrite string
	}{
		{
			name:    "cd into a repository before the candidate",
			cwd:     root,
			command: "cd " + server + "; WT=" + root + "/worktrees/server-review-fix-pr-11534; git worktree add --detach \"$WT\" " + oid + " && cd $WT && cat test",
			rewrite: "cd " + server + "; WT=" + root + "/worktrees/server-review-fix-pr-11534; WT=$(wx new --commit " + oid + `) && echo "WT=$WT" && cd $WT && cat test`,
		},
		{
			name:    "directory option after a pipeline",
			cwd:     root,
			command: "git -C server fetch origin -q 2>&1 | tail -2; ls worktrees 2>/dev/null; WT=" + root + "/worktrees/server-check; git -C server worktree add --detach \"$WT\" 31e62bf37726",
			rewrite: "git -C server fetch origin -q 2>&1 | tail -2; ls worktrees 2>/dev/null; WT=" + root + "/worktrees/server-check; WT=$(cd '" + server + `' && wx new --commit 31e62bf37726) && echo "WT=$WT"`,
		},
		{
			name:    "command substitution before the candidate",
			cwd:     server,
			command: `WT=$(mktemp -d) && git worktree add --detach "$WT" origin/main && cd "$WT" && make test`,
			rewrite: `WT=$(mktemp -d) && WT=$(wx new --branch main) && echo "WT=$WT" && cd "$WT" && make test`,
		},
		{
			name:    "braced variable",
			cwd:     server,
			command: `git worktree add -q --detach ${W} HEAD && ls ${W}`,
			rewrite: `W=$(wx new) && echo "W=$W" && ls ${W}`,
		},
		{
			name:    "workspace root then directory option",
			cwd:     server,
			command: `cd .. && git -C server worktree add "$WT" ` + oid + ` && cd "$WT"`,
			rewrite: `cd .. && WT=$(cd '` + server + `' && wx new --commit ` + oid + `) && echo "WT=$WT" && cd "$WT"`,
		},
		{
			name:    "heredoc after the candidate",
			cwd:     server,
			command: "git worktree add \"$WT\" HEAD && cat <<'EOF'\nnote\nEOF",
			rewrite: "WT=$(wx new) && echo \"WT=$WT\" && cat <<'EOF'\nnote\nEOF",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := classifyWorktreeAddCommand(test.command, worktreeAddLocation{cwd: test.cwd, root: root})
			if verdict.decision != worktreeAddRewrite || !verdict.compound || verdict.command != test.rewrite {
				t.Fatalf("verdict=(decision=%d compound=%v command=%q reason=%q), want %q", verdict.decision, verdict.compound, verdict.command, verdict.reason, test.rewrite)
			}
		})
	}
}

// TestClassifyWorktreeAddCommandKeepsUnsafeCompoundsDenied は、失敗時の後続・実行先・残る worktree 作成を
// 静的に決められない連結を、書き換えずに拒否することを確認する。
func TestClassifyWorktreeAddCommandKeepsUnsafeCompoundsDenied(t *testing.T) {
	root, outside := compoundFixture(t)
	server := filepath.Join(root, "server")
	tests := []struct {
		name, cwd, command string
	}{
		{name: "cd out of the workspace", cwd: root, command: "cd " + filepath.Join(outside, "repo") + ` && WT=/tmp/x && git worktree add --detach "$WT" 3139d39`},
		{name: "cd to an expanded directory", cwd: root, command: `cd "$REPO" && git worktree add "$WT" HEAD && cd "$WT"`},
		{name: "relative cd without cwd", cwd: "", command: `cd server && git worktree add "$WT" HEAD`},
		{name: "cd to a directory outside any repository", cwd: server, command: `cd .. && git worktree add "$WT" HEAD`},
		{name: "conditional cd followed by semicolon", cwd: root, command: `test -d server && cd server; git worktree add "$WT" HEAD && cd "$WT"`},
		{name: "cd after or", cwd: root, command: `false || cd server && git worktree add "$WT" HEAD`},
		{name: "pipe after the candidate", cwd: server, command: `git worktree add --detach "$WT" 3139d39 2>&1 | tail -1 && cd "$WT"`},
		{name: "semicolon after the candidate", cwd: server, command: `git worktree add --detach "$WT" 3139d39; cd "$WT"`},
		{name: "or before the candidate", cwd: server, command: `false || git worktree add "$WT" HEAD && cd "$WT"`},
		{name: "literal path", cwd: server, command: "git worktree add x && cd x"},
		{name: "path built from a variable", cwd: server, command: `git worktree add --detach $S/wt 3139d39 && cd $S/wt`},
		{name: "reserved variable", cwd: server, command: `git worktree add "$HOME" HEAD && ls`},
		{name: "subshell around the candidate", cwd: server, command: `(git worktree add "$WT" HEAD) && cd "$WT"`},
		{name: "subshell with cd", cwd: root, command: `true && (cd server && git worktree add "$WT" HEAD)`},
		{name: "control flow before the candidate", cwd: server, command: `for d in a; do :; done; git worktree add "$WT" HEAD && cd "$WT"`},
		{name: "two candidates", cwd: server, command: `git worktree add "$A" HEAD && git worktree add "$B" HEAD`},
		{name: "candidate in a substitution", cwd: server, command: `git worktree add "$WT" HEAD && echo "$(git worktree add y)"`},
		{name: "candidate behind a heredoc", cwd: server, command: "git worktree add \"$WT\" HEAD && cat <<'EOF'\nnote\nEOF\ngit worktree add y"},
		{name: "eval after the candidate", cwd: server, command: `git worktree add "$WT" HEAD && eval "$NEXT"`},
		{name: "nested shell after the candidate", cwd: server, command: `git worktree add "$WT" HEAD && sh -c 'git worktree add y'`},
		{name: "branch creation", cwd: server, command: `git worktree add -b topic "$WT" HEAD && cd "$WT"`},
		{name: "directory option outside the workspace", cwd: server, command: "git -C " + filepath.Join(outside, "repo") + ` worktree add "$WT" HEAD && cd "$WT"`},
		{name: "redirect on the candidate", cwd: server, command: `git worktree add "$WT" HEAD >/dev/null && cd "$WT"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := classifyWorktreeAddCommand(test.command, worktreeAddLocation{cwd: test.cwd, root: root})
			if verdict.decision != worktreeAddDeny || verdict.reason != denyReasonCompound {
				t.Fatalf("verdict=(decision=%d command=%q reason=%q), want a compound deny", verdict.decision, verdict.command, verdict.reason)
			}
		})
	}
}

// TestPreToolUseDecisionChecksBranchAttachAfterCompoundRewrite は、連結を書き換えて allow を返す前に
// 書き換え後の command をブランチ attach の判定へ通すことを確認する。
func TestPreToolUseDecisionChecksBranchAttachAfterCompoundRewrite(t *testing.T) {
	root, _ := compoundFixture(t)
	server := filepath.Join(root, "server")
	decide := func(t *testing.T, command string, managed bool) preToolUseHookOutput {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"cwd": server, "tool_input": map[string]any{"command": command, "description": "lease"}})
		if err != nil {
			t.Fatal(err)
		}
		output, ok := preToolUseDecision(context.Background(), payload, root, managed)
		if !ok {
			t.Fatalf("command %q produced no decision", command)
		}
		return output
	}
	attach := `git worktree add --detach "$WT" HEAD && cd "$WT" && git checkout -b topic`
	if output := decide(t, attach, true); output.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("managed attach after rewrite=%+v, want deny", output.HookSpecificOutput)
	}
	// wx 外の直接起動は従来どおりブランチ attach を判定しない。
	if output := decide(t, attach, false); output.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("direct attach after rewrite=%+v, want allow", output.HookSpecificOutput)
	}
	output := decide(t, `git worktree add --detach "$WT" HEAD && cd "$WT" && make test`, true)
	specific := output.HookSpecificOutput
	var command string
	if err := json.Unmarshal(specific.UpdatedInput["command"], &command); err != nil {
		t.Fatal(err)
	}
	if specific.PermissionDecision != "allow" || command != `WT=$(wx new) && echo "WT=$WT" && cd "$WT" && make test` {
		t.Fatalf("rewrite=%+v command=%q", specific, command)
	}
	if specific.AdditionalContext != compoundRewriteContext || string(specific.UpdatedInput["description"]) != `"lease"` {
		t.Fatalf("rewrite context=%q updatedInput=%v", specific.AdditionalContext, specific.UpdatedInput)
	}
	if !strings.Contains(specific.AdditionalContext, "NAME=<path>") {
		t.Fatalf("compound context=%q does not explain the printed path", specific.AdditionalContext)
	}
}

// TestRewrittenPayloadKeepsThePayloadWhenItCannotBeDecoded は、組み立てられない payload を元のまま返すことを確認する。
func TestRewrittenPayloadKeepsThePayloadWhenItCannotBeDecoded(t *testing.T) {
	if got := string(rewrittenPayload([]byte("{"), map[string]json.RawMessage{"command": json.RawMessage(`"x"`)})); got != "{" {
		t.Fatalf("rewrittenPayload=%q, want the original payload", got)
	}
}

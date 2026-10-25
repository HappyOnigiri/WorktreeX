package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzHookPayload(f *testing.F) {
	for _, seed := range []string{
		`{"session_id":"abc","source":"resume","cwd":"/tmp/wx"}`,
		`{"session_id":"abc"}`,
		`   `,
		``,
		`null`,
		`{`,
		`[1,2,3]`,
		`{"session_id":123}`,
		`{"cwd":"/tmp/wx\nfake"}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"git worktree add --detach x HEAD"}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"git worktree add x feature"}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"git -C other worktree add x"}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"sh -c \"git worktree add x\""}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"grep -rn \"worktree add\" ."}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"git worktree add '"}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"git worktree add x $(git rev-parse HEAD)"}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"git worktree add --detach /abs/wt 3139d39"}}`,
		`{"cwd":"/wx/slot","tool_input":{"command":"WT=/x; git worktree add --detach \"$WT\" 3139d39 && cd \"$WT\""}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// 1MiB 超は decodeHookPayload が切り詰めるため round trip の対象にならず、実行速度だけを落とす。
		if len(data) > 1<<20 {
			t.Skip()
		}
		checkWorktreeAddHookOutput(t, data)
		if command, err := json.Marshal(string(data)); err == nil {
			checkWorktreeAddHookOutput(t, []byte(`{"cwd":"/wx/slot","tool_input":{"command":`+string(command)+`}}`))
		}
		payload, err := decodeHookPayload(strings.NewReader(string(data)))
		if err != nil {
			if payload != (HookInput{}) {
				t.Fatalf("decode failed but returned payload %+v", payload)
			}
			return
		}
		if len(strings.TrimSpace(string(data))) == 0 && payload != (HookInput{}) {
			t.Fatalf("blank input produced payload %+v", payload)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := decodeHookPayload(strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatalf("re-decode of %q: %v", encoded, err)
		}
		if roundTrip != payload {
			t.Fatalf("round trip=%+v, want %+v", roundTrip, payload)
		}
	})
}

// checkWorktreeAddHookOutput は pre-tool-use の判定が満たすべき不変条件を確かめる。
// 書き換え先は次の shell がそのまま解釈するため、引用の要る文字を含んではいけない。
func checkWorktreeAddHookOutput(t *testing.T, payload []byte) {
	t.Helper()
	output, ok := worktreeAddHookOutput(payload, "/wx/slot")
	if !ok {
		if output.SystemMessage != "" || output.HookSpecificOutput.PermissionDecision != "" {
			t.Fatalf("ignored payload returned output %+v", output)
		}
		return
	}
	specific := output.HookSpecificOutput
	if specific.HookEventName != "PreToolUse" || specific.PermissionDecisionReason == "" {
		t.Fatalf("incomplete decision %+v", specific)
	}
	switch specific.PermissionDecision {
	case "deny":
		if specific.UpdatedInput != nil {
			t.Fatalf("deny decision carries updatedInput %v", specific.UpdatedInput)
		}
	case "allow":
		var command string
		if err := json.Unmarshal(specific.UpdatedInput["command"], &command); err != nil {
			t.Fatalf("updatedInput has no string command: %v", err)
		}
		if specific.AdditionalContext == compoundRewriteContext {
			checkCompoundRewrite(t, command)
			return
		}
		// fuzz の cwd と root は実在しないので、git -C を cd へ写す書き換えは起こらない。
		if !strings.HasPrefix(command, "wx new") {
			t.Fatalf("rewritten command=%q", command)
		}
		if strings.ContainsAny(command, "\"'`$\\;&|<>()\n\r\t") {
			t.Fatalf("rewritten command needs quoting: %q", command)
		}
		fields := strings.Fields(command)
		if len(fields) != 2 && (len(fields) != 4 || (fields[2] != "--branch" && fields[2] != "--commit")) {
			t.Fatalf("rewritten command has an unexpected shape: %q", command)
		}
	default:
		t.Fatalf("unexpected permissionDecision %q", specific.PermissionDecision)
	}
}

// checkCompoundRewrite は連結の書き換えが切り貼りで構文を壊さず、git worktree add を残さないことを確かめる。
func checkCompoundRewrite(t *testing.T, command string) {
	t.Helper()
	if !strings.Contains(command, "=$(wx new") {
		t.Fatalf("compound rewrite has no wx new assignment: %q", command)
	}
	tokens, ok := lexPolicyCommand(command)
	if !ok {
		t.Fatalf("compound rewrite is not well formed: %q", command)
	}
	if candidates, _ := scanCompoundTokens(tokens); candidates != 0 {
		t.Fatalf("compound rewrite still runs git worktree add: %q", command)
	}
}

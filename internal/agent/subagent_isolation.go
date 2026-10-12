package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

const (
	subagentIsolationSystemMessage = `wx blocked Agent(isolation="worktree"); lease a workspace with wx new.`
	subagentIsolationReason        = `wx prepares the worktrees in a wx session. isolation="worktree" leaves a worktree that wx does not manage, ` +
		"and its worktree-agent-* branch, registered in the source repository, and it skips the .worktreelink symlinks (such as .tools), " +
		"so the subagent has to rebuild its environment. " +
		"Run `wx new` once per subagent, write the absolute path it prints into the prompt with an instruction to work in that directory, " +
		"and call Agent without isolation. Dropping isolation alone makes the subagent share this working tree, " +
		"and subagents running in parallel then collide on their edits."
	// wx 外で起動した session には lease を返す session 終了が無いため、id を控えて返却させる。
	unmanagedSubagentIsolationReason = `This repository is set up for wx worktrees. isolation="worktree" leaves a worktree that wx does not manage, ` +
		"and its worktree-agent-* branch, registered in the source repository, and it skips the .worktreelink symlinks (such as .tools), " +
		"so the subagent has to rebuild its environment. " +
		"Run `wx new --json` once per subagent, write the absolute path it prints into the prompt with an instruction to work in that directory, " +
		"and call Agent without isolation. Dropping isolation alone makes the subagent share this working tree, " +
		"and subagents running in parallel then collide on their edits. " +
		"This session was not started by wx, so its end does not return the leases: " +
		"run `wx release <session_id>` with the session_id printed by wx new when each subagent finishes."
)

// subagentTools は SubAgent を起動するツール名である。Task は Agent へ改称される前の名前。
var subagentTools = map[string]bool{"Agent": true, "Task": true}

// subagentIsolationPayload は Agent の呼び出しのうち判定に使う部分だけを持つ。
// 型の合わない値は json.Unmarshal が失敗させ、呼び出し側が fail-open で通す。
type subagentIsolationPayload struct {
	ToolName  string `json:"tool_name"`
	CWD       string `json:"cwd"`
	ToolInput struct {
		Isolation string `json:"isolation"`
	} `json:"tool_input"`
}

// subagentIsolationHookOutput は wx session での Agent(isolation="worktree") を deny する。
// prompt に isolation や worktree の語が入るのは普通なので、判定は tool_input.isolation の値だけで行う。
// remote はローカルに worktree を作らないので通し、読めない payload も通す。
func subagentIsolationHookOutput(payload []byte) (preToolUseHookOutput, bool) {
	if _, ok := worktreeIsolationCWD(payload); !ok {
		return preToolUseHookOutput{}, false
	}
	return denyPreToolUse(subagentIsolationReason, subagentIsolationSystemMessage), true
}

// unmanagedSubagentIsolationHookOutput は wx 外で起動した session での Agent(isolation="worktree") を、
// cwd の workspace の保存済み方針が hot か cold のときだけ deny する。
// off / ask や方針を引けない cwd では wx new が使えるとは限らないため通す。
func unmanagedSubagentIsolationHookOutput(ctx context.Context, payload []byte, worktreeMode func(context.Context, string) (string, bool)) (preToolUseHookOutput, bool) {
	cwd, ok := worktreeIsolationCWD(payload)
	if !ok {
		return preToolUseHookOutput{}, false
	}
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return preToolUseHookOutput{}, false
		}
		cwd = wd
	}
	if mode, resolved := worktreeMode(ctx, cwd); !resolved || (mode != "hot" && mode != "cold") {
		return preToolUseHookOutput{}, false
	}
	return denyPreToolUse(unmanagedSubagentIsolationReason, subagentIsolationSystemMessage), true
}

// worktreeIsolationCWD は payload が Agent(isolation="worktree") の呼び出しなら、その cwd を返す。
func worktreeIsolationCWD(payload []byte) (string, bool) {
	// 全ツール呼び出しで走るため、関係しない payload は JSON を解く前に落とす。
	if !strings.Contains(string(payload), `"isolation"`) {
		return "", false
	}
	var decoded subagentIsolationPayload
	if json.Unmarshal(payload, &decoded) != nil {
		return "", false
	}
	if !subagentTools[decoded.ToolName] || decoded.ToolInput.Isolation != "worktree" {
		return "", false
	}
	return decoded.CWD, true
}

// configuredWorktreeMode は cwd の policy root に対する保存済み worktree 方針を返す。
// 設定の読み込みや root の解決に失敗したら判定しない。
func configuredWorktreeMode(ctx context.Context, cwd string) (string, bool) {
	cfg, err := config.Load()
	if err != nil {
		return "", false
	}
	discoverer := discovery.Discoverer{Git: &gitx.Runner{Timeout: cfg.System.Discovery.Timeout.Duration}, Config: cfg}
	root, err := discoverer.PolicyRoot(ctx, cwd)
	if err != nil {
		return "", false
	}
	return cfg.WorktreeMode(root), true
}

// denyPreToolUse は updatedInput を持たない deny を組み立てる。
func denyPreToolUse(reason, systemMessage string) preToolUseHookOutput {
	return preToolUseHookOutput{
		HookSpecificOutput: preToolUseHookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: reason,
		},
		SystemMessage: systemMessage,
	}
}

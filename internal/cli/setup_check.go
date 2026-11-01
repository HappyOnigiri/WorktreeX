package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

const setupCheckAgentKind = "wx-setup-check"

// SetupCheckResult は RunSetupCheck の結果である。
// Lease と Repositories は検査まで進めた場合だけ入り、貸出前の失敗では空のままになる。
type SetupCheckResult struct {
	Findings     []diag.Finding
	Lease        daemon.Lease
	Repositories []daemon.SetupCheckRepository
	Inspected    bool
}

// SavePrompt は検査結果から確認プロンプトを保存し、保存先の path を stdout へ表示する。
// 検査まで進めなかった場合は保存できる内容がないため何もせず false を返す。
func (c Client) SavePrompt(result SetupCheckResult) bool {
	if !result.Inspected {
		return false
	}
	localizer := cliLocalizer(c)
	prompt, err := c.renderSetupPrompt(result.Lease, result.Repositories, result.Findings)
	if err == nil {
		var path string
		if path, err = setupPromptSaver(prompt); err == nil {
			if _, err = fmt.Fprintln(os.Stdout, localizer.Localize("cli.setup_prompt.saved", map[string]any{"Path": path})); err == nil {
				_, err = fmt.Fprintln(os.Stdout, localizer.Localize("cli.setup_prompt.run", map[string]any{"Path": path}))
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, localizer.Localize("cli.setup_prompt.failed", map[string]any{"Error": err.Error()}))
		return false
	}
	return true
}

// RunSetupCheck は指定 workspace を通常どおり貸し出し、保存経路を保ったまま初回検査と同じ確認を行う。
func (c Client) RunSetupCheck(ctx context.Context, root string) SetupCheckResult {
	if err := c.ensureDaemon(ctx); err != nil {
		return SetupCheckResult{Findings: []diag.Finding{setupMeasurementFinding(root, err.Error())}}
	}
	ownerID, ownerToken := leaseOwnerFromEnvironment()
	params := rpc.ResolveAndLeaseParams{
		Agent: setupCheckAgentKind, ClientPID: 0, CWD: root, ForceWorktree: c.forceWorktree,
		LeaseKind: state.LeaseKindPath, LeaseOwnerSessionID: ownerID, LeaseOwnerToken: ownerToken,
		Language: c.Config.LanguageForRPC(),
	}
	leaseCtx, cancelLease := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancelLease()
	var lease daemon.Lease
	if err := c.RPC.Call(leaseCtx, "ResolveAndLease", params, &lease); err != nil {
		return SetupCheckResult{Findings: []diag.Finding{probeLeaseProblem(root, newProbeStage(probeStageLease, err))}}
	}
	defer c.releaseLeaseToken(lease, "setup-check")
	if !lease.Ready {
		readinessTimeout := leaseReadinessTimeout(c.Config, lease)
		waitCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		err := c.RPC.Call(waitCtx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(readinessTimeout.Milliseconds())}, nil)
		cancel()
		if err != nil {
			return SetupCheckResult{Findings: []diag.Finding{probePrepareProblem(root, lease.Path, newProbeStage(probeStageFullReady, err))}}
		}
	}
	_, findings := c.inspectLeasedWorkspace(ctx, lease.SourceWorkspace, lease.SessionID, lease.Path, true)
	if len(lease.SetupCheckRepositories) > 0 {
		now := time.Now().UTC().Format(time.RFC3339)
		_ = recordInitialSetup(lease.SetupCheckRepositories, lease.SourceWorkspace, now, "")
	}
	return SetupCheckResult{Findings: findings, Lease: lease, Repositories: lease.SetupCheckRepositories, Inspected: true}
}

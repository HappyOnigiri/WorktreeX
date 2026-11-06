package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// READY standby は submodule ごと実体化され、post-checkout hook より前に揃っている。
// hook が submodule の中身を前提にする運用を成立させるため、挿入位置も hook 自身で固定する。
func TestReadyStandbyMaterializesSubmoduleBeforePostCheckout(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, func(t *testing.T, repository string) {
		initGitRepoWithSubmodule(t, repository)
		writeSubmoduleGuardHook(t, repository)
	})
	standby := f.readyStandby(t)
	worktree := f.standbyWorktree(t, standby)
	submodule := filepath.Join(worktree, daemonSubmodulePath)
	if _, err := os.Stat(filepath.Join(submodule, "tracked.txt")); err != nil {
		t.Fatalf("standby submodule content: %v", err)
	}
	gitlink := gitOutput(t, worktree, "rev-parse", "HEAD:"+daemonSubmodulePath)
	if head := gitOutput(t, submodule, "rev-parse", "HEAD"); head != gitlink {
		t.Fatalf("standby submodule HEAD=%s, want gitlink %s", head, gitlink)
	}
	if status := gitOutput(t, worktree, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("standby status=%q, want clean", status)
	}
}

// slot を削除すると per-worktree の管理ディレクトリごと回収され、submodule の gitdir も残らない。
// wx は `.git` 配下を個別に削除する経路を持たないため、この一手だけが後始末になる。
func TestRemoveRegisteredSlotReclaimsSubmoduleGitdir(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, initGitRepoWithSubmodule)
	ctx := context.Background()
	standby := f.readyStandby(t)
	worktree := f.standbyWorktree(t, standby)
	common := gitOutput(t, f.repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	name := worktreeAdminName(t, common, worktree)
	if _, err := os.Stat(filepath.Join(common, "worktrees", name, "modules", daemonSubmoduleName)); err != nil {
		t.Fatalf("per-worktree submodule gitdir: %v", err)
	}
	if _, scheduled, err := f.store.ScheduleRemoval(ctx, standby.ID, ""); err != nil || !scheduled {
		t.Fatalf("schedule removal scheduled=%t err=%v", scheduled, err)
	}
	// 補充まで走らせると同じ管理ディレクトリ名で新しいslotが登録され、回収できたかを見分けられない。
	f.runPendingJobsOfKind(t, "REMOVE")
	if _, err := os.Stat(filepath.Join(common, "worktrees", name)); !os.IsNotExist(err) {
		t.Fatalf("worktree admin directory stat err=%v, want it removed with the slot", err)
	}
}

// writeSubmoduleGuardHook は submodule が空のまま post-checkout に到達したら失敗する hook を置く。
func writeSubmoduleGuardHook(t *testing.T, repository string) {
	t.Helper()
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	hook := filepath.Join(common, "hooks", "post-checkout")
	script := "#!/bin/sh\ntest -e " + daemonSubmodulePath + "/tracked.txt || { echo 'submodule is empty at post-checkout' >&2; exit 1; }\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (f *reuseStandbyFixture) standbyWorktree(t *testing.T, slot state.Slot) string {
	t.Helper()
	repositoryState, err := f.store.SlotRepository(context.Background(), slot.ID, string(f.workspace.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	return repositoryState.WorktreePath
}

// worktreeAdminName は target を指す `<common dir>/worktrees/<name>` の name を backlink から引く。
func worktreeAdminName(t *testing.T, common, target string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(common, "worktrees", entry.Name(), "gitdir"))
		if err != nil {
			continue
		}
		if filepath.Clean(string(data[:len(data)-1])) == filepath.Join(target, ".git") {
			return entry.Name()
		}
	}
	t.Fatalf("no worktree admin directory points at %s", target)
	return ""
}

// advanceGitlink は upstream の child に commit を積み、main のローカル module へ取り込んでから gitlink を進める。
// 返り値は gitlink を進めた main の OID と child の新しい commit である。
func (f *reuseStandbyFixture) advanceGitlink(t *testing.T) (string, string) {
	t.Helper()
	child := filepath.Join(filepath.Dir(f.repository), "child")
	if err := os.WriteFile(filepath.Join(child, "tracked.txt"), []byte("child ahead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, child, "commit", "-am", "child ahead")
	ahead := strings.TrimSpace(gitOutput(t, child, "rev-parse", "HEAD"))
	checkout := filepath.Join(f.repository, daemonSubmodulePath)
	gitRun(t, checkout, "-c", "protocol.file.allow=always", "fetch", "origin")
	gitRun(t, checkout, "checkout", "--detach", ahead)
	gitRun(t, f.repository, "add", daemonSubmodulePath)
	gitRun(t, f.repository, "commit", "-m", "advance gitlink")
	return strings.TrimSpace(gitOutput(t, f.repository, "rev-parse", "HEAD")), ahead
}

// requireSyncedSubmodule は standby が newOID へ更新され、子が新しい gitlink の commit に揃っていることを確かめる。
func (f *reuseStandbyFixture) requireSyncedSubmodule(t *testing.T, slotID, newOID, ahead string) {
	t.Helper()
	repositoryState, err := f.store.SlotRepository(context.Background(), slotID, string(f.workspace.Repositories[0].ID))
	if err != nil || repositoryState.BaseOID != newOID {
		t.Fatalf("updated repository=%+v err=%v, want base %s", repositoryState, err, newOID)
	}
	worktree := repositoryState.WorktreePath
	if head := strings.TrimSpace(gitOutput(t, filepath.Join(worktree, daemonSubmodulePath), "rev-parse", "HEAD")); head != ahead {
		t.Fatalf("submodule HEAD=%s after update, want the new gitlink %s", head, ahead)
	}
	if status := gitOutput(t, worktree, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("updated standby status=%q, want clean", status)
	}
}

// gitlink だけが進んだ main への idle 更新は、子をローカル module から同期して READY のまま保つ。
// 同期できないと gitlink を進める commit のたびに待機枠を作り直すことになる。
func TestIdleStandbyRefreshSyncsAdvancedGitlink(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, initGitRepoWithSubmodule)
	ctx := context.Background()
	standby := f.readyStandby(t)
	newOID, ahead := f.advanceGitlink(t)
	f.manager.reconcileRegistry(ctx)
	updates := f.pendingUpdateJobs(t)
	if len(updates) != 1 || updates[0].SlotID != standby.ID || updates[0].SessionID != "" {
		t.Fatalf("update jobs=%+v, want one session-less UPDATE of %s", updates, standby.ID)
	}
	f.runPendingJobsOfKind(t, "UPDATE")
	if got := f.slotState(t, standby.ID); got != "READY" {
		t.Fatalf("refreshed standby state=%s, want READY", got)
	}
	f.requireSyncedSubmodule(t, standby.ID, newOID, ahead)
}

// 貸出付きの更新も同じ同期を通り、gitlink だけが進んだ main に対して同じ slot を貸し出す。
func TestLeasedStandbyUpdateSyncsAdvancedGitlink(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, initGitRepoWithSubmodule)
	ctx := context.Background()
	standby := f.readyStandby(t)
	newOID, ahead := f.advanceGitlink(t)
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID != standby.ID || lease.Route != RouteUpdate {
		t.Fatalf("lease=%+v, want the standby update route on %s", lease, standby.ID)
	}
	f.runPendingJobsOfKind(t, "UPDATE")
	if err := f.manager.WaitReady(ctx, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	f.requireSyncedSubmodule(t, standby.ID, newOID, ahead)
}

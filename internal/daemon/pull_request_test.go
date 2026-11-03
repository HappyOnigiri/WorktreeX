package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

func localPullRequestOrigin(t *testing.T, repo string) (string, string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitRun(t, repo, "init", "--bare", origin)
	gitRun(t, repo, "remote", "add", "origin", "https://github.com/owner/repository.git")
	gitRun(t, repo, "config", "url."+origin+".insteadOf", "https://github.com/owner/repository.git")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "push", "origin", "HEAD:refs/heads/main", "HEAD:refs/pull/7/head")
	return origin, base
}

func TestPullRequestLeaseDetachedAndRestore(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t, func(s *managerFixtureSetup) { s.Config.Worktree.FetchDefaultBranch = true })
	origin, base := localPullRequestOrigin(t, repo)
	gitRun(t, repo, "commit", "--allow-empty", "-m", "advance source")
	source := gitOutput(t, repo, "rev-parse", "HEAD")
	handler := Handler{Manager: f.Manager}
	raw, err := json.Marshal(rpc.ResolveAndLeaseParams{Agent: "wx-path", CWD: repo, LeaseKind: state.LeaseKindPath, PullRequests: []string{"7"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.dispatch(context.Background(), "ResolveAndLease", raw)
	if err != nil {
		t.Fatal(err)
	}
	lease := result.(Lease)
	ctx := context.Background()
	if err := waitReady(ctx, f.Manager, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, lease.Path, "rev-parse", "HEAD"); got != base {
		t.Fatalf("HEAD=%s want %s", got, base)
	}
	if got := gitOutput(t, lease.Path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Fatalf("attached HEAD=%s", got)
	}
	if got := gitOutput(t, repo, "rev-parse", "HEAD"); got != source {
		t.Fatal("source HEAD changed")
	}
	entries, err := f.Store.SlotRepositories(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].RequestedRef != "https://github.com/owner/repository/pull/7" || entries[0].BaseOID != base {
		t.Fatalf("entries=%+v", entries)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "restored.txt"), []byte("saved PR work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Manager.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		session, _ := f.Store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	// PR ref を削除しても、復元は保存済み OID と snapshot から行う。
	gitRun(t, repo, "push", origin, ":refs/pull/7/head")
	restored, err := f.Manager.Resume(ctx, lease.SessionID, "wx-path", 0, false, ResumeOptions{Lease: leaseAttrs{Kind: state.LeaseKindPath}})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, f.Manager, 10*time.Second, restored.SessionID, restored.Token); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(restored.Path, "restored.txt"))
	if err != nil || string(data) != "saved PR work\n" {
		t.Fatalf("restored data=%q err=%v", data, err)
	}
	if got := gitOutput(t, restored.Path, "rev-parse", "HEAD"); got != base {
		t.Fatalf("restored HEAD=%s", got)
	}
	if _, err := handler.dispatch(ctx, "ResolveAndLease", raw); err == nil {
		t.Fatal("missing PR fetched cached head")
	}
}

func TestPullRequestLeaseKeepsDefaultStandby(t *testing.T) {
	t.Parallel()
	f := newReuseStandbyFixture(t)
	ctx := context.Background()
	ready := f.readyStandby(t)
	_, base := localPullRequestOrigin(t, f.repository)
	gitRun(t, f.repository, "commit", "--allow-empty", "-m", "advance PR")
	gitRun(t, f.repository, "push", "origin", "HEAD:refs/pull/7/head")
	f.manager.cfg.Worktree.ReuseStandby = false
	lease, err := f.manager.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{PullRequests: []string{"7"}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SessionID == ready.ID {
		t.Fatal("used mismatching default standby")
	}
	slot, err := f.store.Slot(ctx, ready.ID)
	if err != nil || slot.State != "READY" {
		t.Fatalf("standby=%+v err=%v", slot, err)
	}
	entries, err := f.store.SlotRepositories(ctx, lease.SessionID)
	if err != nil || len(entries) != 1 || entries[0].BaseOID == base || !strings.Contains(entries[0].RequestedRef, "/pull/7") {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
}

func TestPullRequestMultiRepositoryLeaseAndFetchFailure(t *testing.T) {
	t.Parallel()
	f, _ := leaseWorktreeFixture(t, func(s *managerFixtureSetup) { s.Config.Worktree.FetchDefaultBranch = true })
	root := filepath.Join(f.Root, "multi")
	a, b, c := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
	for _, repo := range []string{a, b, c} {
		initGitRepo(t, repo)
	}
	localPullRequestOrigin(t, a)
	bOrigin, _ := localPullRequestOrigin(t, b)
	gitRun(t, c, "branch", "feature")
	ctx := context.Background()
	// 最初の PR の取得後に別 PR が失敗しても、slot・session・準備 job は作らない。
	gitRun(t, b, "push", bOrigin, ":refs/pull/7/head")
	_, err := f.Manager.ResolveAndLease(ctx, root, []string{"c=feature"}, "codex", 0, leaseAttrs{PullRequests: []string{"a=7", "b=7"}})
	if err == nil {
		t.Fatal("missing second PR accepted")
	}
	raw := openTestDatabase(t, f.DatabasePath)
	for _, table := range []string{"slots", "sessions", "jobs"} {
		var count int
		if err := raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("failed fetch created %d %s", count, table)
		}
	}
	if refs := gitOutput(t, a, "for-each-ref", "refs/wx/pr"); refs == "" {
		t.Fatal("first PR was not fetched")
	}
	gitRun(t, b, "push", bOrigin, "HEAD:refs/pull/7/head")
	lease, err := f.Manager.ResolveAndLease(ctx, root, nil, "codex", 0, leaseAttrs{PullRequests: []string{"a=7", "b=7"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, f.Manager, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	entries, err := f.Store.SlotRepositories(ctx, lease.SessionID)
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	defaultCount := 0
	for _, entry := range entries {
		path := filepath.Join(lease.Path, entry.DirName)
		if got := gitOutput(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
			t.Fatalf("attached worktree: %s", path)
		}
		if entry.RequestedRef == "main" {
			defaultCount++
			if entry.BaseOID != gitOutput(t, c, "rev-parse", "HEAD") {
				t.Fatalf("default base=%+v", entry)
			}
		}
	}
	if defaultCount != 1 {
		t.Fatalf("default repositories=%d want 1", defaultCount)
	}
	// repository 内からの番号指定は、その repository 単独の workspace になる。
	single, err := f.Manager.ResolveAndLease(ctx, a, nil, "codex", 0, leaseAttrs{PullRequests: []string{"7"}})
	if err != nil {
		t.Fatal(err)
	}
	singleEntries, err := f.Store.SlotRepositories(ctx, single.SessionID)
	if err != nil || len(singleEntries) != 1 {
		t.Fatalf("entries=%+v err=%v", singleEntries, err)
	}
}

func TestPullRequestLeaseForwardsCallerSSHEnvironment(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	origin, base := localPullRequestOrigin(t, repo)
	gitRun(t, repo, "remote", "set-url", "origin", "git@github.com:owner/repository.git")
	gitRun(t, repo, "config", "core.sshCommand", "/usr/bin/false")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "ssh")
	content := "#!/bin/sh\n[ \"$SSH_AUTH_SOCK\" = caller ] || exit 91\nexec " + shellQuote(realGit) + " upload-pack " + shellQuote(origin) + "\n"
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	handler := Handler{Manager: f.Manager}
	params := rpc.ResolveAndLeaseParams{Agent: "wx-path", CWD: repo, LeaseKind: state.LeaseKindPath, PullRequests: []string{"7"}, SSHEnvironment: []string{"GIT_SSH_COMMAND=" + shellQuote(script), "GIT_SSH_VARIANT=ssh", "SSH_AUTH_SOCK=caller"}}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := handler.dispatch(context.Background(), "ResolveAndLease", raw)
	if err != nil {
		t.Fatal(err)
	}
	lease := result.(Lease)
	if err := waitReady(context.Background(), f.Manager, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, lease.Path, "rev-parse", "HEAD"); got != base {
		t.Fatalf("HEAD=%s want %s", got, base)
	}
	params.SSHEnvironment = []string{"GIT_DIR=/another/repository"}
	raw, err = json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handler.dispatch(context.Background(), "ResolveAndLease", raw); err == nil {
		t.Fatal("RPC accepted repository environment override")
	}
}

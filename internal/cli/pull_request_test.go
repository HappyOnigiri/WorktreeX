package cli

import (
	"context"
	"reflect"
	"testing"
)

func TestPullRequestsReachLeaseRequests(t *testing.T) {
	for _, kind := range []string{"path", "shell", "command"} {
		t.Run(kind, func(t *testing.T) {
			client, handler, base, ctx := leaseFixture(t)
			prs := []string{"a=7", "b=8"}
			client = client.WithPullRequests(prs)
			var exit int
			switch kind {
			case "path":
				exit = client.RunLeaseNewFrom(ctx, base, nil, nil, true)
			case "shell":
				client.Config.System.Lease.Shell = "/usr/bin/true"
				exit = client.RunLeaseShellFrom(ctx, base, nil, "")
			case "command":
				exit = client.RunLeaseCommandFrom(ctx, base, []string{"/usr/bin/true"}, nil, "")
			}
			if exit != 0 {
				t.Fatalf("exit=%d", exit)
			}
			if got := leaseRequest(t, handler).PullRequests; !reflect.DeepEqual(got, prs) {
				t.Fatalf("PRs=%v want %v", got, prs)
			}
		})
	}
}

func TestPullRequestsRejectResumeFreshAndDisabledWorktrees(t *testing.T) {
	for _, test := range []struct {
		args    []string
		fresh   bool
		options WorktreeOptions
	}{
		{args: []string{"resume", "session"}}, {args: []string{"resume"}}, {args: []string{"exec", "resume"}}, {fresh: true}, {options: WorktreeOptions{Disable: true}},
	} {
		client := Client{}.WithPullRequests([]string{"7"})
		if exit := client.RunAgentWithPolicyFrom(context.Background(), t.TempDir(), "codex", test.args, nil, test.fresh, test.options); exit != 2 {
			t.Fatalf("test=%+v exit=%d", test, exit)
		}
	}
	client, handler, base, ctx := leaseFixture(t)
	client = client.WithPullRequests([]string{"7"})
	client.Config.Worktree.Undefined = "off"
	// 保存済み方針が off でも、PR 指定で worktree を有効にしない。
	if exit := client.RunAgentWithPolicyFrom(ctx, base, "codex", nil, nil, false, WorktreeOptions{}); exit != 2 {
		t.Fatalf("off exit=%d", exit)
	}
	if exit := client.RunLeaseShellFrom(ctx, base, nil, "session"); exit != 2 {
		t.Fatalf("resume exit=%d", exit)
	}
	if exit := client.RunLeaseNewFrom(ctx, base, nil, []string{"abcdef1"}, true); exit != 2 {
		t.Fatalf("commit exit=%d", exit)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.leaseParams) > 0 {
		t.Fatalf("invalid request reached daemon: %s", handler.leaseParams)
	}
}

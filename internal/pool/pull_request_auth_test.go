package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// SSH 設定が異なる要求を同じ Runner に渡しても、daemon の環境や他の要求へ混ざらない。
func TestPullRequestsUseRequestScopedSSHEnvironment(t *testing.T) {
	repo := pullRequestRepo(t, t.TempDir(), "a")
	origin := pullRequestOrigin(t, repo, "owner/a")
	git(t, string(repo.MainPath), "remote", "set-url", "origin", "git@github.com:owner/a.git")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_TEST_REAL_GIT", realGit)
	t.Setenv("WX_TEST_PR_ORIGIN", origin)
	t.Setenv("GIT_SSH_COMMAND", "/usr/bin/false")
	t.Setenv("SSH_AUTH_SOCK", "daemon-socket")
	script := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
[ "$SSH_AUTH_SOCK" = "$1" ] || exit 91
exec "$WX_TEST_REAL_GIT" upload-pack "$WX_TEST_PR_ORIGIN"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &gitx.Runner{}
	w := discovery.Workspace{Repositories: []discovery.Repository{repo}}
	want := gitOut(t, string(repo.MainPath), "rev-parse", "HEAD")
	var wg sync.WaitGroup
	for _, socket := range []string{"caller-a", "caller-b"} {
		wg.Go(func() {
			command := "'" + script + "' " + socket
			env := []string{"GIT_SSH_COMMAND=" + command, "GIT_SSH_VARIANT=ssh", "SSH_AUTH_SOCK=" + socket}
			bases, err := ResolvePullRequests(context.Background(), runner, w, nil, nil, []string{"7"}, env)
			if err != nil {
				t.Error(err)
				return
			}
			if bases[0].OID != want {
				t.Errorf("OID=%s want %s", bases[0].OID, want)
			}
		})
	}
	wg.Wait()
	if os.Getenv("GIT_SSH_COMMAND") != "/usr/bin/false" || os.Getenv("SSH_AUTH_SOCK") != "daemon-socket" {
		t.Fatal("request changed daemon environment")
	}
	if _, err := ResolvePullRequests(context.Background(), runner, w, nil, nil, []string{"7"}, nil); err == nil {
		t.Fatal("caller SSH environment leaked into a later request")
	}
	if _, err := ResolvePullRequests(context.Background(), runner, w, nil, nil, []string{"7"}, []string{"GIT_DIR=/another/repository"}); err == nil {
		t.Fatal("unsafe repository override accepted")
	}
}

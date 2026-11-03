package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

func pullRequestRepo(t *testing.T, root, name string) discovery.Repository {
	t.Helper()
	path := initRepo(t, filepath.Join(root, name))
	return discovery.Repository{ID: domain.RepositoryID(name), MainPath: domain.CanonicalPath(path), CommonDir: domain.CanonicalPath(filepath.Join(path, ".git")), RelativePath: name, DefaultBranch: "main"}
}

// ローカルの bare repository を GitHub remote の代わりに使い、通信なしで PR ref を取得する。
func pullRequestOrigin(t *testing.T, repo discovery.Repository, identity string) string {
	t.Helper()
	path := string(repo.MainPath)
	origin := filepath.Join(filepath.Dir(path), repo.RelativePath+"-origin.git")
	git(t, path, "init", "--bare", origin)
	remoteURL := "https://github.com/" + identity + ".git"
	git(t, path, "remote", "add", "origin", remoteURL)
	git(t, path, "config", "url."+origin+".insteadOf", remoteURL)
	git(t, path, "push", "origin", "HEAD:refs/heads/main", "HEAD:refs/pull/7/head")
	return origin
}

func TestPullRequestNormalization(t *testing.T) {
	for _, raw := range []string{"https://github.com/Owner/Repo.git", "git@github.com:Owner/Repo.git", "ssh://git@github.com/Owner/Repo.git", "git@GITHUB.COM:Owner/Repo.git", "github.com:Owner/Repo.git"} {
		if identity, ok := githubRepository(raw); !ok || identity != "owner/repo" {
			t.Errorf("%s: %s %v", raw, identity, ok)
		}
	}
	for _, raw := range []string{"https://enterprise.example/owner/repo", "git@alias:owner/repo.git", "file:///owner/repo", "https://github.com/owner/repo/extra", "https://github.com:123/owner/repo"} {
		if _, ok := githubRepository(raw); ok {
			t.Errorf("accepted %s", raw)
		}
	}
	identity, number, err := pullRequestURL("https://github.com/Owner/Repo/pull/007/?query=x=y#fragment")
	if err != nil || identity != "owner/repo" || number != "7" {
		t.Fatalf("URL: %s %s %v", identity, number, err)
	}
	for _, raw := range []string{"https://github.com/o/r/pull/0", "https://github.com/o/r/pull/-1", "https://github.com/o/r/pull/+1", "https://github.com/o/r/pull/1/files", "http://github.com/o/r/pull/1", "https://enterprise.example/o/r/pull/1", "https://github.com/o/r/pull/18446744073709551616"} {
		if _, _, err := pullRequestURL(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestPullRequestsFetchLatestHeadAndPreserveSource(t *testing.T) {
	root := t.TempDir()
	repo := pullRequestRepo(t, root, "a")
	path := string(repo.MainPath)
	origin := pullRequestOrigin(t, repo, "owner/a")
	// PR head は通常の branch に無い fork 相当の object でも取得できる。
	fork := initRepo(t, filepath.Join(root, "fork"))
	git(t, fork, "commit", "--allow-empty", "-m", "fork PR")
	first := gitOut(t, fork, "rev-parse", "HEAD")
	git(t, fork, "push", origin, "+HEAD:refs/pull/7/head")
	git(t, path, "tag", "source-tag")
	git(t, path, "config", "remote.origin.tagOpt", "--tags")
	git(t, path, "config", "remote.origin.fetch", "+refs/pull/*:refs/heads/pull/*")
	before := gitOut(t, path, "show-ref")
	head := gitOut(t, path, "rev-parse", "HEAD")
	index, err := os.ReadFile(filepath.Join(path, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("source dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetchHead := filepath.Join(path, ".git", "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &gitx.Runner{}
	w := discovery.Workspace{Repositories: []discovery.Repository{repo}}
	resolve := func() []Resolved {
		t.Helper()
		bases, err := ResolvePullRequests(context.Background(), runner, w, nil, nil, []string{"7"})
		if err != nil {
			t.Fatal(err)
		}
		return bases
	}
	bases := resolve()
	if bases[0].OID != first || bases[0].RequestedRef != "https://github.com/owner/a/pull/7" {
		t.Fatalf("bases=%+v", bases)
	}
	git(t, fork, "checkout", "--detach", "HEAD~1")
	second := gitOut(t, fork, "rev-parse", "HEAD")
	git(t, fork, "push", origin, "+HEAD:refs/pull/7/head")
	if got := resolve()[0].OID; got != second {
		t.Fatalf("force push OID=%s want %s", got, second)
	}
	if bases[0].OID != first {
		t.Fatal("prior resolution changed")
	}
	if got := gitOut(t, path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("source HEAD changed: %s", got)
	}
	data, _ := os.ReadFile(filepath.Join(path, ".git", "index"))
	if string(data) != string(index) {
		t.Fatal("index changed")
	}
	data, _ = os.ReadFile(filepath.Join(path, "tracked.txt"))
	if string(data) != "source dirty\n" {
		t.Fatal("tracked file changed")
	}
	data, _ = os.ReadFile(fetchHead)
	if string(data) != "unchanged\n" {
		t.Fatal("FETCH_HEAD changed")
	}
	var refs []string
	for _, line := range strings.Split(gitOut(t, path, "show-ref"), "\n") {
		if !strings.Contains(line, " refs/wx/pr/") {
			refs = append(refs, line)
		}
	}
	if strings.Join(refs, "\n") != before {
		t.Fatal("source refs changed")
	}
	git(t, fork, "push", origin, ":refs/pull/7/head")
	if _, err := ResolvePullRequests(context.Background(), runner, w, nil, nil, []string{"7"}); err == nil {
		t.Fatal("missing PR used cached head")
	}
}

func TestPullRequestsCombineRepositoriesAndQualifiedBranch(t *testing.T) {
	root := t.TempDir()
	a, b, c := pullRequestRepo(t, root, "a"), pullRequestRepo(t, root, "b"), pullRequestRepo(t, root, "c")
	pullRequestOrigin(t, a, "owner/a")
	pullRequestOrigin(t, b, "owner/b")
	git(t, string(c.MainPath), "config", "remote.pushonly.pushurl", "git@example.com:owner/c.git")
	git(t, string(c.MainPath), "branch", "feature")
	w := discovery.Workspace{Repositories: []discovery.Repository{a, b, c}}
	for _, specs := range [][]string{{"a=7", "https://github.com/owner/b/pull/7"}, {"https://github.com/owner/a/pull/7/?x=y#z", "https://github.com/owner/b/pull/7"}} {
		bases, err := ResolvePullRequests(context.Background(), &gitx.Runner{}, w, []string{"c=feature"}, nil, specs)
		if err != nil {
			t.Fatal(err)
		}
		if len(bases) != 3 || bases[2].RequestedRef != "feature" {
			t.Fatalf("bases=%+v", bases)
		}
	}
	bases, err := ResolvePullRequests(context.Background(), &gitx.Runner{}, w, nil, nil, []string{"a=7"})
	if err != nil || bases[1].RequestedRef != "main" || bases[2].RequestedRef != "main" {
		t.Fatalf("default bases=%+v err=%v", bases, err)
	}
}

func TestPullRequestsValidateAllTargetsBeforeFetching(t *testing.T) {
	root := t.TempDir()
	a, b := pullRequestRepo(t, root, "a"), pullRequestRepo(t, root, "b")
	pullRequestOrigin(t, a, "owner/a")
	pullRequestOrigin(t, b, "owner/b")
	w := discovery.Workspace{Repositories: []discovery.Repository{a, b}}
	for _, test := range []struct{ branches, commits, prs []string }{
		{prs: []string{"7"}},
		{prs: []string{"a=7", "a=7"}},
		{prs: []string{"a=7", "https://github.com/owner/a/pull/7"}},
		{prs: []string{"a=7", "b=0"}},
		{prs: []string{"a=7", "missing=7"}},
		{prs: []string{"a=7", "https://github.com/owner/unknown/pull/7"}},
		{branches: []string{"main"}, prs: []string{"a=7"}},
		{branches: []string{"a=main"}, prs: []string{"a=7"}},
		{branches: []string{"b=missing"}, prs: []string{"a=7"}},
		{commits: []string{"1234567"}, prs: []string{"a=7"}},
	} {
		if _, err := ResolvePullRequests(context.Background(), &gitx.Runner{}, w, test.branches, test.commits, test.prs); err == nil {
			t.Errorf("accepted %+v", test)
		}
		if refs := gitOut(t, string(a.MainPath), "for-each-ref", "refs/wx/pr"); refs != "" {
			t.Fatalf("fetched before validation: %s", refs)
		}
	}
}

func TestPullRequestRemoteSelection(t *testing.T) {
	a := discovery.Repository{ID: "a", RelativePath: "a"}
	b := discovery.Repository{ID: "b", RelativePath: "b"}
	github := pullRequestRemote{name: "upstream", repository: "owner/a"}
	origin := pullRequestRemote{name: "origin", repository: "owner/b"}
	for _, test := range []struct {
		name, spec, want string
		repos            []discovery.Repository
		remotes          map[string][]pullRequestRemote
	}{
		{name: "origin wins for number", spec: "7", want: "origin", repos: []discovery.Repository{a}, remotes: map[string][]pullRequestRemote{"a": {github, origin}}},
		{name: "URL selects upstream", spec: "https://github.com/owner/a/pull/7", want: "upstream", repos: []discovery.Repository{a}, remotes: map[string][]pullRequestRemote{"a": {github, origin}}},
		{name: "unique non-origin", spec: "7", want: "upstream", repos: []discovery.Repository{a}, remotes: map[string][]pullRequestRemote{"a": {github}}},
		{name: "no GitHub remote", spec: "7", repos: []discovery.Repository{a}},
		{name: "ambiguous remotes", spec: "7", repos: []discovery.Repository{a}, remotes: map[string][]pullRequestRemote{"a": {github, {name: "other", repository: "owner/a"}}}},
		{name: "ambiguous repositories", spec: "https://github.com/owner/a/pull/7", repos: []discovery.Repository{a, b}, remotes: map[string][]pullRequestRemote{"a": {github}, "b": {github}}},
		{name: "URL origin tie break", spec: "https://github.com/owner/a/pull/7", want: "origin", repos: []discovery.Repository{a}, remotes: map[string][]pullRequestRemote{"a": {github, {name: "origin", repository: "owner/a"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, err := resolvePullRequestTarget(discovery.Workspace{Repositories: test.repos}, test.remotes, test.spec)
			if test.want == "" {
				if err == nil {
					t.Fatal("ambiguous target accepted")
				}
				return
			}
			if err != nil || target.remote.name != test.want {
				t.Fatalf("target=%+v err=%v", target, err)
			}
		})
	}
}

type pullRequestLockWaiter struct{ waiting chan struct{} }

func (w pullRequestLockWaiter) Release()                     { close(w.waiting) }
func (w pullRequestLockWaiter) Acquire(context.Context) bool { return true }

func TestPullRequestsConcurrentResolutionPinsOID(t *testing.T) {
	root := t.TempDir()
	repo := pullRequestRepo(t, root, "a")
	origin := pullRequestOrigin(t, repo, "owner/a")
	first := gitOut(t, string(repo.MainPath), "rev-parse", "HEAD")
	git(t, string(repo.MainPath), "commit", "--allow-empty", "-m", "next PR head")
	next := gitOut(t, string(repo.MainPath), "rev-parse", "HEAD")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "reading")
	gate := filepath.Join(bin, "continue")
	t.Setenv("WX_TEST_REAL_GIT", realGit)
	t.Setenv("WX_TEST_PR_MARKER", marker)
	t.Setenv("WX_TEST_PR_GATE", gate)
	script := `#!/bin/sh
case "$1:$4" in
 rev-parse:refs/wx/pr/*)
  if mkdir "$WX_TEST_PR_MARKER" 2>/dev/null; then
   while [ ! -f "$WX_TEST_PR_GATE" ]; do sleep 0.01; done
  fi
  ;;
esac
exec "$WX_TEST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() { _ = os.WriteFile(gate, nil, 0o600) }()
	runner := &gitx.Runner{}
	w := discovery.Workspace{Repositories: []discovery.Repository{repo}}
	type result struct {
		bases []Resolved
		err   error
	}
	one, two := make(chan result, 1), make(chan result, 1)
	go func() {
		bases, err := ResolvePullRequests(ctx, runner, w, nil, nil, []string{"7"})
		one <- result{bases, err}
	}()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case got := <-one:
			t.Fatalf("first request returned before read barrier: %+v", got)
		case <-ctx.Done():
			t.Fatal("first request did not reach read barrier")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 最初の fetch が完了した後で remote を進め、別要求の fetch を競合させる。
	git(t, string(repo.MainPath), "push", origin, "+HEAD:refs/pull/7/head")
	waiting := make(chan struct{})
	secondCtx := gitx.WithLockWaiter(ctx, pullRequestLockWaiter{waiting: waiting})
	go func() {
		bases, err := ResolvePullRequests(secondCtx, runner, w, nil, nil, []string{"7"})
		two <- result{bases, err}
	}()
	select {
	case <-waiting:
	case got := <-two:
		t.Fatalf("second request did not wait for the OID read: %+v", got)
	case <-ctx.Done():
		t.Fatal("second request did not wait on common-directory lock")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		done chan result
		want string
	}{{one, first}, {two, next}} {
		select {
		case got := <-test.done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.bases[0].OID != test.want {
				t.Fatalf("OID=%s want %s", got.bases[0].OID, test.want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := ResolvePullRequests(canceled, runner, w, nil, nil, []string{"7"}); err == nil {
		t.Fatal("canceled request succeeded")
	}
}

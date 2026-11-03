package pool

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

type (
	pullRequestRemote struct{ name, repository string }
	pullRequestTarget struct {
		repo   discovery.Repository
		remote pullRequestRemote
		number string
	}
)

// ResolvePullRequests は対象と起点の競合をすべて検証してから PR head を取得する。
// 取得済み ref へは後退せず、fetch と OID の固定を同じ common-directory lock 内で終える。
func ResolvePullRequests(ctx context.Context, git *gitx.Runner, w discovery.Workspace, branches, commits, specs []string) ([]Resolved, error) {
	if len(commits) > 0 {
		return nil, fmt.Errorf("--pr cannot be combined with --commit")
	}
	occupied := map[string]bool{}
	for _, spec := range branches {
		selector, _, qualified := strings.Cut(spec, "=")
		if !qualified {
			return nil, fmt.Errorf("--branch requires repo=branch when combined with --pr")
		}
		repo, err := selectRepository(w.Repositories, "branch", selector)
		if err != nil {
			return nil, err
		}
		occupied[string(repo.ID)] = true
	}
	remotes := map[string][]pullRequestRemote{}
	for _, repo := range w.Repositories {
		found, err := githubRemotes(ctx, git, repo)
		if err != nil {
			return nil, err
		}
		remotes[string(repo.ID)] = found
	}
	var targets []pullRequestTarget
	selected := map[string]bool{}
	for _, spec := range specs {
		target, err := resolvePullRequestTarget(w, remotes, spec)
		if err != nil {
			return nil, err
		}
		id := string(target.repo.ID)
		if occupied[id] || selected[id] {
			return nil, fmt.Errorf("repository %q has multiple start point specifications", target.repo.RelativePath)
		}
		selected[id] = true
		targets = append(targets, target)
	}
	remaining := w
	remaining.Repositories = nil
	for _, repo := range w.Repositories {
		if !selected[string(repo.ID)] {
			remaining.Repositories = append(remaining.Repositories, repo)
		}
	}
	// 未指定 repository の解決失敗も、PR fetch より前に検出する。
	resolved, err := ResolveBranches(ctx, git, remaining, branches)
	if err != nil {
		return nil, err
	}
	byID := map[string]Resolved{}
	for _, base := range resolved {
		byID[string(base.Repository.ID)] = base
	}
	for _, target := range targets {
		base, err := fetchPullRequest(ctx, git, target)
		if err != nil {
			return nil, err
		}
		byID[string(target.repo.ID)] = base
	}
	out := make([]Resolved, 0, len(w.Repositories))
	for _, repo := range w.Repositories {
		out = append(out, byID[string(repo.ID)])
	}
	return out, nil
}

func githubRemotes(ctx context.Context, git *gitx.Runner, repo discovery.Repository) ([]pullRequestRemote, error) {
	result, err := git.Run(ctx, string(repo.MainPath), "remote")
	if err != nil {
		return nil, err
	}
	var out []pullRequestRemote
	for _, name := range strings.Fields(result.Stdout) {
		result, err := git.Run(ctx, string(repo.MainPath), "config", "--get-all", "remote."+name+".url")
		if err != nil {
			var gitErr *gitx.Error
			if ctx.Err() == nil && errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 && gitErr.Result.Stderr == "" {
				continue
			}
			return nil, err
		}
		// fetch が使う先頭の URL を照合し、SSH alias や URL 書換えの解決は行わない。
		rawURL, _, _ := strings.Cut(strings.TrimSpace(result.Stdout), "\n")
		if identity, ok := githubRepository(rawURL); ok {
			out = append(out, pullRequestRemote{name: name, repository: identity})
		}
	}
	return out, nil
}

// githubRepository は SSH host alias を解決せず、github.com の remote だけを照合する。
func githubRepository(raw string) (string, bool) {
	if !strings.Contains(raw, "://") {
		authority, path, scp := strings.Cut(raw, ":")
		_, host, user := strings.Cut(authority, "@")
		if !user {
			host = authority
		}
		if scp && strings.EqualFold(host, "github.com") {
			raw = "ssh://" + authority + "/" + path
		}
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if port := u.Port(); port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "ssh" && port == "22") {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return strings.ToLower(parts[0] + "/" + parts[1]), true
}

func pullRequestURL(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil {
		return "", "", fmt.Errorf("invalid GitHub pull request URL %q", raw)
	}
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] == "" || parts[2] == "" || parts[3] != "pull" {
		return "", "", fmt.Errorf("invalid GitHub pull request URL %q", raw)
	}
	number, err := pullRequestNumber(parts[4])
	return strings.ToLower(parts[1] + "/" + parts[2]), number, err
}

func pullRequestNumber(raw string) (string, error) {
	for _, c := range raw {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("invalid pull request number %q; use a positive integer", raw)
		}
	}
	number, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || number == 0 {
		return "", fmt.Errorf("invalid pull request number %q; use a positive integer", raw)
	}
	return strconv.FormatUint(number, 10), nil
}

func resolvePullRequestTarget(w discovery.Workspace, remotes map[string][]pullRequestRemote, spec string) (pullRequestTarget, error) {
	var repo discovery.Repository
	var identity, number string
	var err error
	if strings.Contains(spec, "://") {
		identity, number, err = pullRequestURL(spec)
		if err != nil {
			return pullRequestTarget{}, err
		}
		var matches []discovery.Repository
		for _, candidate := range w.Repositories {
			for _, remote := range remotes[string(candidate.ID)] {
				if remote.repository == identity {
					matches = append(matches, candidate)
					break
				}
			}
		}
		if len(matches) != 1 {
			return pullRequestTarget{}, fmt.Errorf("pull request URL %q matches %d repositories; expected one", spec, len(matches))
		}
		repo = matches[0]
	} else {
		selector, raw, qualified := strings.Cut(spec, "=")
		switch {
		case qualified:
			repo, err = selectRepository(w.Repositories, "pull request", selector)
		case len(w.Repositories) == 1:
			repo = w.Repositories[0]
			raw = spec
		default:
			return pullRequestTarget{}, fmt.Errorf("pull request number requires repo=number in a multi-repository workspace")
		}
		if err != nil {
			return pullRequestTarget{}, err
		}
		number, err = pullRequestNumber(raw)
		if err != nil {
			return pullRequestTarget{}, err
		}
	}
	var candidates []pullRequestRemote
	for _, remote := range remotes[string(repo.ID)] {
		if identity == "" || remote.repository == identity {
			candidates = append(candidates, remote)
		}
	}
	for _, remote := range candidates {
		if remote.name == "origin" {
			return pullRequestTarget{repo: repo, remote: remote, number: number}, nil
		}
	}
	if len(candidates) != 1 {
		return pullRequestTarget{}, fmt.Errorf("repository %q has %d matching GitHub remotes; expected origin or one remote", repo.RelativePath, len(candidates))
	}
	return pullRequestTarget{repo: repo, remote: candidates[0], number: number}, nil
}

func fetchPullRequest(ctx context.Context, git *gitx.Runner, target pullRequestTarget) (Resolved, error) {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte("github.com/"+target.remote.repository)))
	ref := "refs/wx/pr/" + identity + "/" + target.number + "/head"
	base := Resolved{Repository: target.repo, RequestedRef: "https://github.com/" + target.remote.repository + "/pull/" + target.number}
	// remote の ref mapping・prune・submodule 設定を継承せず、専用 ref だけを更新する。
	err := git.WithCommonDirLock(ctx, string(target.repo.CommonDir), func(locked context.Context) error {
		_, err := git.Run(locked, string(target.repo.MainPath), "fetch", "--no-tags", "--no-write-fetch-head", "--refmap=", "--no-prune", "--no-recurse-submodules", "--", target.remote.name, "+refs/pull/"+target.number+"/head:"+ref)
		if err != nil {
			return err
		}
		oid, ok, err := resolveExactRef(locked, git, string(target.repo.MainPath), ref)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("fetched PR head is missing")
		}
		base.OID = oid
		return nil
	})
	if err != nil {
		return Resolved{}, fmt.Errorf("fetch %s in repository %s: %w", base.RequestedRef, target.repo.RelativePath, err)
	}
	return base, nil
}

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClassifyWorktreeAddCommandResolvesDirectory は git -C を workspace 内の repository に限って受け付け、
// そのディレクトリで wx new を実行する形へ写すことを確認する。
func TestClassifyWorktreeAddCommandResolvesDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"server/.git", "server/cmd", "web", "notrepo", "it's/.git", "home/repo/.git"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// linked worktree と同じく、.git がファイルの repository も受け付ける。
	if err := os.WriteFile(filepath.Join(root, "web", ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "repo", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "repo"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	server := filepath.Join(root, "server")
	tests := []struct {
		name, cwd, command, rewrite, reason string
	}{
		{name: "relative repository from workspace root", cwd: root, command: "git -C server worktree add --detach /abs/wt origin/main", rewrite: "(cd '" + server + "' && wx new --branch main)"},
		{name: "absolute repository with object name", cwd: root, command: "git -C " + server + " worktree add --detach /abs/wt 9aa38f56fb24247307dd28795d2fef393d9da3ee", rewrite: "(cd '" + server + "' && wx new --commit 9aa38f56fb24247307dd28795d2fef393d9da3ee)"},
		{name: "same directory as cwd", cwd: server, command: "git -C . worktree add x", rewrite: "wx new"},
		{name: "sibling repository", cwd: server, command: "git -C ../web worktree add x main", rewrite: "(cd '" + filepath.Join(root, "web") + "' && wx new --branch main)"},
		{name: "subdirectory of a repository", cwd: root, command: "git -C server/cmd worktree add x", rewrite: "(cd '" + filepath.Join(server, "cmd") + "' && wx new)"},
		{name: "quote in directory name", cwd: root, command: `git -C "it's" worktree add x`, rewrite: "(cd '" + root + `/it'\''s' && wx new)`},
		{name: "home directory", cwd: root, command: "git -C ~/repo worktree add x", rewrite: "(cd '" + filepath.Join(root, "home", "repo") + "' && wx new)"},
		{name: "quoted tilde", cwd: root, command: `git -C "~/repo" worktree add x`, reason: denyReasonDirectory},
		{name: "directory without repository", cwd: root, command: "git -C notrepo worktree add x", reason: denyReasonDirectory},
		{name: "missing directory", cwd: root, command: "git -C missing worktree add x", reason: denyReasonDirectory},
		{name: "repository outside workspace", cwd: root, command: "git -C " + filepath.Join(outside, "repo") + " worktree add x", reason: denyReasonDirectory},
		{name: "symlink out of workspace", cwd: root, command: "git -C link worktree add x", reason: denyReasonDirectory},
		{name: "glob", cwd: root, command: "git -C serv* worktree add x", reason: denyReasonDirectory},
		{name: "relative directory without cwd", cwd: "", command: "git -C server worktree add x", reason: denyReasonDirectory},
		{name: "absolute directory without cwd", cwd: "", command: "git -C " + server + " worktree add x", rewrite: "(cd '" + server + "' && wx new)"},
		// 引数の判定を先に済ませるので、書き換えられない引数は -C より先に理由になる。
		{name: "branch creation with directory", cwd: root, command: "git -C missing worktree add -b topic x", reason: denyReasonBranchCreation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := classifyWorktreeAddCommand(test.command, worktreeAddLocation{cwd: test.cwd, root: root})
			if verdict.command != test.rewrite || verdict.reason != test.reason {
				t.Fatalf("verdict=(decision=%d command=%q reason=%q), want command %q reason %q", verdict.decision, verdict.command, verdict.reason, test.rewrite, test.reason)
			}
		})
	}
}

// TestClassifyWorktreeAddCommandRefusesDirectoryWithoutRoot は境界が分からないときに -C を受け付けないことを確認する。
func TestClassifyWorktreeAddCommandRefusesDirectoryWithoutRoot(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", filepath.Join(repository, "missing")} {
		verdict := classifyWorktreeAddCommand("git -C "+repository+" worktree add x", worktreeAddLocation{cwd: repository, root: root})
		if verdict.reason != denyReasonDirectory {
			t.Fatalf("root %q verdict=%+v, want %q", root, verdict, denyReasonDirectory)
		}
	}
}

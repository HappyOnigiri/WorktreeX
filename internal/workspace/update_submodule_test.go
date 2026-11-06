package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// advanceSubmoduleGitlink は upstream の child に commit を積み、main のローカル module へ取り込んでから gitlink を進める。
// change は child の worktree に加える変更で、返り値は gitlink を進めた main の OID と child の新しい commit である。
func (f *submoduleFixture) advanceSubmoduleGitlink(t *testing.T, change func(child string)) (string, string) {
	t.Helper()
	change(f.child)
	gitCommand(t, f.child, "add", "-A")
	gitCommand(t, f.child, "commit", "-m", "child ahead")
	ahead := gitOutput(t, f.child, "rev-parse", "HEAD")
	checkout := filepath.Join(f.repository, submodulePath)
	gitCommand(t, checkout, "-c", "protocol.file.allow=always", "fetch", "origin")
	gitCommand(t, checkout, "checkout", "--detach", ahead)
	gitCommand(t, f.repository, "add", submodulePath)
	gitCommand(t, f.repository, "commit", "-m", "advance gitlink")
	return gitOutput(t, f.repository, "rev-parse", "HEAD"), ahead
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// commit だけが進んだ gitlink は、ローカル module から子を同期して更新する。
// fetch 元はローカル module に限り、子の origin とソースリポジトリの設定を書き換えない。
func TestUpdateSyncsAdvancedGitlinkFromLocalModule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSubmoduleFixture(t)
	if err := f.preparer.Prepare(ctx, f.repo, f.target, f.head, testSlotID); err != nil {
		t.Fatal(err)
	}
	origin := gitOutput(t, f.submoduleTarget(), "remote", "get-url", "origin")
	newOID, ahead := f.advanceSubmoduleGitlink(t, func(child string) {
		writeTestFile(t, filepath.Join(child, "kid.txt"), "ahead\n")
		writeTestFile(t, filepath.Join(child, "added.txt"), "added\n")
	})
	sourceConfig := readTestFile(t, filepath.Join(string(f.repo.CommonDir), "config"))
	moduleConfig := readTestFile(t, filepath.Join(f.moduleDir(), "config"))
	if err := validateCollisionCandidate(ctx, f.preparer, f.repo, f.target, f.head, newOID, nil, nil); err != nil {
		t.Fatalf("advanced gitlink candidate: %v", err)
	}
	if _, err := f.preparer.UpdateLocked(ctx, f.repo, f.target, f.head, newOID, testSlotID, nil, nil); err != nil {
		t.Fatalf("advanced gitlink update: %v", err)
	}
	if head := gitOutput(t, f.submoduleTarget(), "rev-parse", "HEAD"); head != ahead {
		t.Fatalf("submodule HEAD=%s after update, want the new gitlink %s", head, ahead)
	}
	if got := readTestFile(t, filepath.Join(f.submoduleTarget(), "added.txt")); string(got) != "added\n" {
		t.Fatalf("submodule added.txt=%q after update", got)
	}
	if status := gitOutput(t, f.target, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("updated worktree status=%q, want clean", status)
	}
	if got := gitOutput(t, f.submoduleTarget(), "remote", "get-url", "origin"); got != origin {
		t.Fatalf("submodule origin=%s after update, want %s", got, origin)
	}
	if got := readTestFile(t, filepath.Join(string(f.repo.CommonDir), "config")); !bytes.Equal(got, sourceConfig) {
		t.Fatalf("source repository config changed:\n%s\nwant:\n%s", got, sourceConfig)
	}
	if got := readTestFile(t, filepath.Join(f.moduleDir(), "config")); !bytes.Equal(got, moduleConfig) {
		t.Fatalf("local module config changed:\n%s\nwant:\n%s", got, moduleConfig)
	}
}

// 子の実体と構成に依存する同期不能条件は、書き込む前に ErrUpdateIneligible で Cold Start へ戻す。
// 判定後に standby の HEAD と子の HEAD が動いていないことも確かめ、隔離へ進む経路を作らないことを固定する。
func TestUpdateRejectsUnsyncableGitlinkChanges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// disableSubmodules は standby を submodule 方針無効で準備し、子を空のまま残す。
		disableSubmodules bool
		// candidateOnly は予約前の検査だけが弾く条件で、更新処理自体は予約前の検査を通った入力を前提にする。
		candidateOnly bool
		// arrange は standby の準備後に main と standby へ加える変更で、要求OIDを返す。
		arrange func(t *testing.T, f *submoduleFixture) string
	}{
		{
			name:          "gitmodules changed",
			candidateOnly: true,
			arrange: func(t *testing.T, f *submoduleFixture) string {
				f.advanceSubmoduleGitlink(t, func(child string) { writeTestFile(t, filepath.Join(child, "kid.txt"), "ahead\n") })
				content := string(readTestFile(t, filepath.Join(f.repository, ".gitmodules"))) + "\tbranch = main\n"
				return f.commitGitmodules(t, content)
			},
		},
		{
			name:              "child not materialized",
			disableSubmodules: true,
			arrange: func(t *testing.T, f *submoduleFixture) string {
				newOID, _ := f.advanceSubmoduleGitlink(t, func(child string) { writeTestFile(t, filepath.Join(child, "kid.txt"), "ahead\n") })
				return newOID
			},
		},
		{
			name: "child attributes changed",
			arrange: func(t *testing.T, f *submoduleFixture) string {
				newOID, _ := f.advanceSubmoduleGitlink(t, func(child string) {
					writeTestFile(t, filepath.Join(child, ".gitattributes"), "*.txt text eol=crlf\n")
				})
				return newOID
			},
		},
		{
			name: "child untracked collision",
			arrange: func(t *testing.T, f *submoduleFixture) string {
				newOID, _ := f.advanceSubmoduleGitlink(t, func(child string) { writeTestFile(t, filepath.Join(child, "added.txt"), "tracked\n") })
				writeTestFile(t, filepath.Join(f.submoduleTarget(), "added.txt"), "local\n")
				return newOID
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newSubmoduleFixture(t)
			policy := f.preparer.Config.RepositoryDefaults.Submodules
			if tc.disableSubmodules {
				disabled := false
				f.preparer.Config.RepositoryDefaults.Submodules = &disabled
			}
			if err := f.preparer.Prepare(ctx, f.repo, f.target, f.head, testSlotID); err != nil {
				t.Fatal(err)
			}
			// 方針を戻しても、無効で準備した standby の子は空のままである。
			f.preparer.Config.RepositoryDefaults.Submodules = policy
			newOID := tc.arrange(t, f)
			childHead := ""
			if !tc.disableSubmodules {
				childHead = gitOutput(t, f.submoduleTarget(), "rev-parse", "HEAD")
			}
			err := validateCollisionCandidate(ctx, f.preparer, f.repo, f.target, f.head, newOID, nil, nil)
			if !errors.Is(err, ErrUpdateIneligible) {
				t.Fatalf("candidate error=%v, want ErrUpdateIneligible", err)
			}
			if !tc.candidateOnly {
				if _, err := f.preparer.UpdateLocked(ctx, f.repo, f.target, f.head, newOID, testSlotID, nil, nil); !errors.Is(err, ErrUpdateIneligible) {
					t.Fatalf("update error=%v, want the pre-write ErrUpdateIneligible", err)
				}
			}
			if head := gitOutput(t, f.target, "rev-parse", "HEAD"); head != f.head {
				t.Fatalf("standby HEAD=%s after rejection, want the untouched %s", head, f.head)
			}
			if childHead != "" {
				if head := gitOutput(t, f.submoduleTarget(), "rev-parse", "HEAD"); head != childHead {
					t.Fatalf("submodule HEAD=%s after rejection, want the untouched %s", head, childHead)
				}
			}
		})
	}
}

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

// gitlinkSync は standby の更新で子を新しいgitlinkへ移す1件で、書込み前の判定で確定する。
// source は clone 元と同じ main 側のローカル module で、fetch もここからだけ行う。
type gitlinkSync struct {
	path   string
	source string
	oid    string
}

// planSubmoduleSync は commit だけが変わった gitlink を、書き込む前に同期できるかを判定する。
// 1件でも満たさなければ ErrUpdateIneligible を返し、更新全体を Cold Start へ戻す。
// 子が tracked clean で HEAD が旧 gitlink に一致することは、呼び出し側の `status --ignore-submodules=untracked` が保証する。
// commentlint:allow-long -- ここで判定しない条件と、その保証の所在を呼び出し側の保守者が確認できるようにする
func (p *Preparer) planSubmoduleSync(ctx context.Context, repo discovery.Repository, target, identity, newOID string, root *os.Root, changes []gitlinkChange) ([]gitlinkSync, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	declared, err := p.declaredSubmodulesAtTree(ctx, string(repo.MainPath), newOID)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]submodule, len(declared))
	for _, module := range declared {
		byPath[module.path] = module
	}
	commonModules := filepath.Join(string(repo.CommonDir), "modules")
	syncs := make([]gitlinkSync, 0, len(changes))
	for _, change := range changes {
		module, ok := byPath[change.path]
		if !ok {
			return nil, fmt.Errorf("%w: gitlink %s is not declared in .gitmodules", ErrUpdateIneligible, change.path)
		}
		// 省略した子は空のまま残っており、更新で実体化すると Cold Start の適格判定を複製することになる。
		if _, err := root.Lstat(filepath.Join(change.path, ".git")); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: submodule %s is not materialized in the standby", ErrUpdateIneligible, change.path)
			}
			return nil, err
		}
		source := filepath.Join(commonModules, module.name)
		if !domain.IsWithin(commonModules, source) {
			return nil, fmt.Errorf("submodule %q resolves outside the source repository module directory", module.name)
		}
		if err := p.rejectUnsyncableSubmodule(ctx, target, identity, root, change, source); err != nil {
			return nil, err
		}
		syncs = append(syncs, gitlinkSync{path: change.path, source: source, oid: change.newOID})
	}
	return syncs, nil
}

// rejectUnsyncableSubmodule は1件の子について、ローカル module と子の worktree に依存する更新不能条件を検査する。
// fetch は clone の local 最適化を通らないため、shallow や promisor の module では書込み後に失敗し得る。
// 新しい commit が揃った通常の module だけを通す。
func (p *Preparer) rejectUnsyncableSubmodule(ctx context.Context, target, identity string, root *os.Root, change gitlinkChange, source string) error {
	inspection, err := InspectSubmodule(ctx, p.Git, source, change.newOID)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w: submodule %s local module could not be inspected: %w", ErrUpdateIneligible, change.path, err)
	}
	if status := inspection.Status(); status != SubmoduleSharingAvailable {
		return fmt.Errorf("%w: submodule %s local module is %s for %s", ErrUpdateIneligible, change.path, status, change.newOID)
	}
	result, err := p.Git.Run(ctx, source, "--git-dir=.", "diff-tree", "-r", "-z", "--raw", "--no-renames", change.oldOID, change.newOID)
	if err != nil {
		if execErr := submoduleInspectionExecutionError(ctx, err); execErr != nil {
			return execErr
		}
		return fmt.Errorf("%w: submodule %s cannot compare %s with %s in the local module: %w", ErrUpdateIneligible, change.path, change.oldOID, change.newOID, err)
	}
	diff, err := parseUpdateTreeDiff(result.Stdout)
	if err != nil {
		return err
	}
	// 子の checkout も stat cache の一致した file を書き直さないため、親と同じく属性の変化を弾く。
	if diff.attributes {
		return fmt.Errorf("%w: .gitattributes changed in submodule %s", ErrUpdateIneligible, change.path)
	}
	child, err := root.OpenRoot(change.path)
	if err != nil {
		return err
	}
	defer func() { _ = child.Close() }()
	probes, err := updateCollisionProbes(child, diff.added)
	if err != nil {
		return err
	}
	// 子の checkout は `--force` を付けないが、ignored file は黙って上書きするため、親と同じく書込み前に弾く。
	untracked, err := p.untrackedPathsUnderAt(ctx, target, identity, []string{"-C", change.path}, probes)
	if err != nil {
		return err
	}
	if len(untracked) > 0 {
		return fmt.Errorf("%w: untracked or ignored path %s conflicts with the update of submodule %s", ErrUpdateIneligible, untracked[0], change.path)
	}
	return nil
}

// syncSubmoduleGitlinks は親の checkout 後に、子を新しい gitlink へ移す。
// `git submodule update` は子に無い commit を子の origin（復元済みの upstream）から取りに行くため使わず、
// ローカル module を明示して fetch する。v2 を固定するのは、広告されない OID の要求を受け付けさせるためである。
// hook を止めるのは、利用者の post-checkout が `git submodule update` を起動してネットワークへ出ないようにするためである。
// commentlint:allow-long -- Git の各指定を外したときに起きる失敗を保守時に確認できるようにする
func (p *Preparer) syncSubmoduleGitlinks(ctx context.Context, target, identity string, syncs []gitlinkSync) error {
	for _, item := range syncs {
		if _, err := p.RunGitInWorktree(ctx, target, identity, nil, nil,
			"-C", item.path, "-c", "protocol.file.allow=always", "-c", "protocol.version=2", "-c", "core.hooksPath=/dev/null",
			"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", item.source, item.oid); err != nil {
			return fmt.Errorf("fetch submodule %s: %w", item.path, err)
		}
		if _, err := p.RunGitInWorktree(ctx, target, identity, nil, nil,
			"-C", item.path, "-c", "core.hooksPath=/dev/null", "checkout", "--quiet", "--detach", "--no-recurse-submodules", item.oid); err != nil {
			return fmt.Errorf("checkout submodule %s: %w", item.path, err)
		}
	}
	return nil
}

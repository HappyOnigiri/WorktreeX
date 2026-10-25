package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// worktreeAddLocation は git -C を解決する起点で、payload の cwd と書き換えの境界である workspace root を持つ。
type worktreeAddLocation struct {
	cwd, root string
}

// repositoryDirectory は git -C の値を、書き換え後の wx new を実行するディレクトリへ解決する。
// 空のディレクトリは -C が無いか cwd と同じ場所を指すことを表し、issue が空でなければ deny の理由である。
// multi-repository workspace でも `repo=` セレクタへは写さず、そのディレクトリで wx new を実行する。
// slot 内のディレクトリ名は source 側の repository 名と一致する保証がなく、git -C が作るのも
// その repository 単独の worktree であり、wx new はリポジトリ内で実行すると単独の repository を貸し出すためである。
// commentlint:allow-long -- セレクタへ写さない理由を残す
func (l worktreeAddLocation) repositoryDirectory(words []shellWord) (directory, issue string) {
	switch {
	case len(words) == 0:
		return "", ""
	case len(words) > 1:
		return "", denyReasonUnsupported
	case words[0].expanded:
		return "", denyReasonExpandedDirectory
	}
	word := words[0]
	if l.cwd == "" && !filepath.IsAbs(word.value) && !word.tilde {
		return "", denyReasonDirectory
	}
	resolved := resolvePolicyDirectory(commandWord{value: word.value, quoted: word.quoted, globbed: word.globbed, tilde: word.tilde}, l.cwd)
	if resolved == "" || l.root == "" {
		return "", denyReasonDirectory
	}
	root, err := filepath.EvalSymlinks(l.root)
	if err != nil || !withinDirectory(root, resolved) || !insideRepository(resolved, root) {
		return "", denyReasonDirectory
	}
	if current, err := filepath.EvalSymlinks(l.cwd); err == nil && current == resolved {
		return "", ""
	}
	return resolved, ""
}

func withinDirectory(root, target string) bool {
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

// insideRepository は target から root までの祖先に `.git` があるかを返す。
// wx new はその位置から git rev-parse --show-toplevel で repository を引くので、toplevel である必要はない。
func insideRepository(target, root string) bool {
	for directory := target; ; directory = filepath.Dir(directory) {
		if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
			return true
		}
		if directory == root || directory == filepath.Dir(directory) {
			return false
		}
	}
}

// shellSingleQuote は POSIX shell の単一引用符で value を囲む。
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

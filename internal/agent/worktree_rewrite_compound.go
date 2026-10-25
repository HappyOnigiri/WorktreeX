package agent

import (
	"path/filepath"
	"regexp"
)

// compoundCommand は連結を区切りで分けた 1 つの単純コマンドである。
// before と after は前後の繋ぎ方で、先頭と末尾は空、それ以外は区切りの原文か `|`・`&` である。
// plain は語だけから成り、リダイレクト・サブシェル・パイプを含まないことを表す。
type compoundCommand struct {
	words         []policyToken
	before, after string
	plain         bool
}

// shellReservedWords は制御構文を始める語で、続く git worktree add が条件付きで走ることを表す。
var shellReservedWords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true, "for": true, "while": true, "until": true,
	"do": true, "done": true, "case": true, "esac": true, "select": true, "function": true, "{": true, "}": true, "!": true,
}

// compoundUnsafeCommands は実行先や実行内容を静的に追えなくするコマンドで、連結のどこにあっても書き換えない。
var compoundUnsafeCommands = map[string]bool{
	"eval": true, "source": true, ".": true, "exec": true, "pushd": true, "popd": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "xargs": true,
}

// compoundPathVariable は書き換えでパスを受け取る変数参照（`$WT` と `${WT}`）である。
var compoundPathVariable = regexp.MustCompile(`^\$(?:([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)\})$`)

// compoundReservedVariables は代入すると後続のコマンドや shell 自体の振る舞いが変わる変数である。
var compoundReservedVariables = map[string]bool{
	"PATH": true, "HOME": true, "PWD": true, "OLDPWD": true, "IFS": true, "CDPATH": true, "SHELL": true, "TMPDIR": true,
}

// rewriteCompoundWorktreeAdd は連結の中の git worktree add 1 つを `NAME=$(wx new ...)` に置き換えた command を返す。
// 書き換えるのは、パス引数が変数 1 つで、その変数を後続へ渡せば元の意図どおりに動く形に限る。
// 置き換えた部分は wx new の終了コードを返し、直後が `&&` か末尾なので、貸出に失敗すれば後続は走らない。
// `;` や `||` で続く形を除くのは、失敗時に空の変数で後続が今の workspace を相手に走るためである。
// 前にある cd は追い、workspace 内に解決できる場合だけ受け付ける。
// commentlint:allow-long -- 受け付ける形と、それ以外を拒否する理由を残す
func rewriteCompoundWorktreeAdd(command string, location worktreeAddLocation) (string, bool) {
	tokens, ok := lexPolicyCommand(command)
	if !ok {
		return "", false
	}
	if candidates, unsafe := scanCompoundTokens(tokens); candidates != 1 || unsafe {
		return "", false
	}
	commands, ok := splitCompoundCommands(tokens)
	if !ok {
		return "", false
	}
	walk := compoundWalk{directory: location.cwd, root: location.root}
	for _, current := range commands {
		if !walk.admit(current) {
			return "", false
		}
		words := policyShellWords(current.words)
		candidate, isCandidate := gitWorktreeAddArguments(words)
		if !isCandidate {
			if !walk.follow(words, current) {
				return "", false
			}
			continue
		}
		// cd で移った先で走る git worktree add は、-C が無ければその位置が repository の中でなければ失敗する。
		if walk.moved && len(candidate.directories) == 0 && !insideRepository(walk.directory, walk.canonicalRoot) {
			return "", false
		}
		replacement, ok := compoundReplacement(candidate, current, worktreeAddLocation{cwd: walk.directory, root: location.root})
		if !ok {
			return "", false
		}
		start, end := current.words[0].start, current.words[len(current.words)-1].end
		return command[:start] + replacement + command[end:], true
	}
	return "", false
}

// scanCompoundTokens は置換の本文まで含めて git worktree add の数を数え、実行内容を静的に追えなくするコマンドの有無を返す。
// allow で返す command に、書き換えなかった git worktree add が残らないようにするためである。
// commentlint:allow-long -- 本文まで数える理由を残す
func scanCompoundTokens(tokens []policyToken) (candidates int, unsafe bool) {
	var words []shellWord
	flush := func() {
		if _, ok := gitWorktreeAddArguments(words); ok {
			candidates++
		}
		index := 0
		for index < len(words) && isAssignmentWord(words[index].value) {
			index++
		}
		if index < len(words) && compoundUnsafeCommands[filepath.Base(words[index].value)] {
			unsafe = true
		}
		words = nil
	}
	for _, token := range tokens {
		if token.kind != policyTokenWord {
			flush()
			continue
		}
		for _, body := range token.word.substitutions {
			bodyCandidates, bodyUnsafe := scanCompoundTokens(body)
			candidates += bodyCandidates
			unsafe = unsafe || bodyUnsafe
		}
		words = append(words, policyShellWord(token.word))
	}
	flush()
	return candidates, unsafe
}

// splitCompoundCommands は token 列を単純コマンドへ分ける。候補より後ろの構文は問わないが、
// 括弧が現れた後は候補が入れ子の中にあり得るので、以降のコマンドを plain にしない。
// 引用されていない `$(...)` の中身は外側のコマンドに数えず、それを含むコマンドを plain にしない。
func splitCompoundCommands(tokens []policyToken) ([]compoundCommand, bool) {
	var commands []compoundCommand
	current := compoundCommand{plain: true}
	nested := false
	substitution := 0
	closeCommand := func(after string) {
		current.after = after
		if len(current.words) > 0 {
			commands = append(commands, current)
		}
		current = compoundCommand{before: after, plain: !nested}
	}
	for _, token := range tokens {
		if substitution > 0 {
			if token.kind == policyTokenOpen {
				substitution++
			} else if token.kind == policyTokenClose {
				substitution--
			}
			continue
		}
		if token.kind == policyTokenOpen && token.operator == "$(" {
			substitution++
			current.plain = false
			continue
		}
		switch token.kind {
		case policyTokenWord:
			current.words = append(current.words, token)
		case policyTokenSeparator:
			closeCommand(token.operator)
		case policyTokenPipe:
			closeCommand("|")
		case policyTokenBackground:
			closeCommand("&")
		case policyTokenOpen, policyTokenClose:
			nested = true
			current.plain = false
		case policyTokenRedirect:
			current.plain = false
		}
	}
	closeCommand("")
	return commands, len(commands) > 0
}

// compoundWalk は候補より前のコマンドを順に読み、候補が走るディレクトリを追う。
type compoundWalk struct {
	directory, root, canonicalRoot string
	// moved は cd で cwd から移ったこと、conditional は最後の cd が `&&` の後にあり走らない場合があることを表す。
	moved, conditional bool
}

// admit はコマンドの前の繋ぎ方が、追っているディレクトリを不確かにしないかを返す。
// `&&` の後の cd は前が失敗すると走らないので、その後に `;` や改行で続くコマンドは cd の有無が分からない。
func (w *compoundWalk) admit(current compoundCommand) bool {
	if !w.moved {
		return true
	}
	switch current.before {
	case "&&", "|":
		return true
	case ";", "\n":
		return !w.conditional
	default:
		return false
	}
}

// follow は候補より前のコマンド 1 つを読む。実行先を変えるコマンドのうち、追えるのは引数 1 つの cd だけである。
func (w *compoundWalk) follow(words []shellWord, current compoundCommand) bool {
	index := 0
	for index < len(words) && isAssignmentWord(words[index].value) {
		index++
	}
	if index >= len(words) {
		return true
	}
	if shellReservedWords[words[index].value] {
		return false
	}
	if filepath.Base(words[index].value) != "cd" {
		return true
	}
	// パイプの中の cd は別プロセスで走り、`||` の後の cd は走るとは限らない。
	if !current.plain || len(words) != index+2 || current.after == "|" || current.after == "&" || current.before == "||" || current.before == "|" {
		return false
	}
	target := words[index+1]
	if w.directory == "" && !filepath.IsAbs(target.value) && !target.tilde {
		return false
	}
	resolved := resolvePolicyDirectory(commandWord{value: target.value, quoted: target.quoted, expanded: target.expanded, globbed: target.globbed, tilde: target.tilde}, w.directory)
	if resolved == "" || w.root == "" {
		return false
	}
	canonicalRoot, err := filepath.EvalSymlinks(w.root)
	if err != nil || !withinDirectory(canonicalRoot, resolved) {
		return false
	}
	w.directory, w.canonicalRoot, w.moved, w.conditional = resolved, canonicalRoot, true, current.before == "&&"
	return true
}

// compoundReplacement は候補 1 つを `NAME=$(wx new ...) && echo "NAME=$NAME"` へ写す。
// 置換の中の stdout は貸出のパスだけなので、echo しないと agent は後のツール呼び出しで使うパスを知り得ない。
func compoundReplacement(candidate worktreeAddCandidate, current compoundCommand, location worktreeAddLocation) (string, bool) {
	switch current.before {
	case "", ";", "&&", "\n":
	default:
		return "", false
	}
	if !current.plain || (current.after != "" && current.after != "&&") {
		return "", false
	}
	verdict := rewriteWorktreeAddCandidate(candidate, location)
	if verdict.decision != worktreeAddRewrite {
		return "", false
	}
	match := compoundPathVariable.FindStringSubmatch(verdict.path.value)
	if match == nil || !verdict.path.expanded {
		return "", false
	}
	name := match[1] + match[2]
	if compoundReservedVariables[name] {
		return "", false
	}
	inner := verdict.command
	if verdict.directory != "" {
		inner = "cd " + shellSingleQuote(verdict.directory) + " && " + inner
	}
	return name + "=$(" + inner + `) && echo "` + name + `=$` + name + `"`, true
}

func policyShellWords(tokens []policyToken) []shellWord {
	words := make([]shellWord, 0, len(tokens))
	for _, token := range tokens {
		words = append(words, policyShellWord(token.word))
	}
	return words
}

func policyShellWord(word commandWord) shellWord {
	return shellWord{value: word.value, expanded: word.expanded, quoted: word.quoted, globbed: word.globbed, tilde: word.tilde}
}

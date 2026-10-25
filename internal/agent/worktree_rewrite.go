package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// preToolUsePayload は pre-tool-use payload のうち判定に使う部分だけを持つ。
// tool_input は未知フィールドを落とさずに書き戻すため、生の JSON のまま保持する。
// HookInput は比較可能である必要があるため（hook_fuzz_test.go の `!=`）、map を足さず型を分ける。
type preToolUsePayload struct {
	CWD       string                     `json:"cwd"`
	ToolInput map[string]json.RawMessage `json:"tool_input"`
}

// preToolUseHookSpecificOutput は Claude Code と Codex が共通で解釈する PreToolUse の応答である。
// updatedInput は文字列の command を持たないと Codex がエラーにするため、deny では省略する。
type preToolUseHookSpecificOutput struct {
	HookEventName            string                     `json:"hookEventName"`
	PermissionDecision       string                     `json:"permissionDecision"`
	PermissionDecisionReason string                     `json:"permissionDecisionReason"`
	AdditionalContext        string                     `json:"additionalContext,omitempty"`
	UpdatedInput             map[string]json.RawMessage `json:"updatedInput,omitempty"`
}

type preToolUseHookOutput struct {
	HookSpecificOutput preToolUseHookSpecificOutput `json:"hookSpecificOutput"`
	SystemMessage      string                       `json:"systemMessage,omitempty"`
}

const (
	rewriteSystemMessage = "wx rewrote git worktree add as wx new."
	denySystemMessage    = "wx blocked git worktree add; use wx new."
	rewriteReason        = "wx leases prepared worktrees, so git worktree add is replaced with wx new."
)

// compoundRewriteContext は連結の一部を書き換えたときの案内で、パスは変数で後続へ渡り、command 自身が表示する。
const compoundRewriteContext = `wx replaced git worktree add inside the command with NAME=$(wx new ...), which leases a workspace that wx prepares, tracks and returns.
- The variable that named the worktree path now holds the leased path for the rest of the command, and the command prints it as NAME=<path>. The path assigned before was never created.
- Use the printed path in later tool calls.
- The leased workspace is detached. Run "git branch <name> HEAD" inside it when the work needs a branch.
- No need to run wx release: the lease is returned when this session ends.
- wx new can take a while when no prepared slot is standing by.`

// rewriteContext は書き換えを受けた agent が次に何をすべきかを伝える。
const rewriteContext = `wx replaced the command with wx new, which leases a workspace that wx prepares, tracks and returns.
- Use the path printed by wx new. wx chooses the path, so the path in the original command was never created.
- The leased workspace is detached. Run "git branch <name> HEAD" inside it when the work needs a branch.
- No need to run wx release: the lease is returned when this session ends.
- wx new can take a while when no prepared slot is standing by.`

// deny の理由は、拒否された形を agent が理由の文だけで特定できるよう、形ごとに分ける。
const (
	denyReasonCompound           = "The command combines git worktree add with other commands, so wx cannot hand the leased path on to them."
	denyReasonOtherRepository    = "--git-dir, --work-tree, --namespace or a leading NAME=value assignment points git at a repository wx cannot verify."
	denyReasonDirectory          = "git -C does not name a repository inside this workspace, and wx leases only repositories of this workspace here."
	denyReasonExpandedDirectory  = "The git -C value uses a variable or command substitution, so wx cannot tell which repository it names."
	denyReasonBranchCreation     = "-b, -B and --orphan create a branch, while wx new leases a detached workspace."
	denyReasonStartPoint         = "The start point is neither a branch name nor a commit object name, which are the only start points wx new accepts."
	denyReasonExpandedStartPoint = "The start point uses a variable or command substitution, so wx cannot tell which commit to lease."
	denyReasonUnsupported        = "wx cannot rewrite this form of git worktree add without guessing what it means."
)

// newCommandGuidance は wx new を単独で実行する案内で、起点の指定方法を含める。複数の理由が共有する。
const newCommandGuidance = "run `wx new` as its own tool call (add `--branch <branch>` or `--commit <oid>` for the start point), then use the path it prints in the tool calls that follow."

// denyGuidance は deny の理由ごとに、agent が次に実行できる形を案内する。
var denyGuidance = map[string]string{
	denyReasonCompound:           "Instead, " + newCommandGuidance + " wx chooses the path, so commands that used the original path must use the printed one.",
	denyReasonOtherRepository:    "Drop the option or assignment, cd into the repository, and " + newCommandGuidance,
	denyReasonDirectory:          "For a repository of this workspace, cd into its directory here and " + newCommandGuidance,
	denyReasonExpandedDirectory:  "Write the git -C directory literally, or cd into it and " + newCommandGuidance,
	denyReasonBranchCreation:     "Instead, " + newCommandGuidance + " Run `git branch <name> HEAD` in that path when the work needs a branch; the workspace itself stays detached.",
	denyReasonStartPoint:         "Resolve the start point to a commit with `git rev-parse`, then run `wx new --commit <oid>` as its own tool call and use the path it prints.",
	denyReasonExpandedStartPoint: "Expand the value first (for example with echo), then run `wx new --branch <branch>` or `wx new --commit <oid>` with the literal value as its own tool call.",
	denyReasonUnsupported:        "Instead, " + newCommandGuidance,
}

type worktreeAddDecision int

const (
	worktreeAddIgnore worktreeAddDecision = iota
	worktreeAddRewrite
	worktreeAddDeny
)

// worktreeAddVerdict の path と directory は書き換えのときだけ入る。
// path は捨てるパス引数の語、directory は git -C から解決した wx new の実行先で、連結の書き換えが使う。
type worktreeAddVerdict struct {
	decision  worktreeAddDecision
	command   string
	reason    string
	path      shellWord
	directory string
	// compound は連結の一部だけを書き換えたことを表し、agent への案内を変える。
	compound bool
}

func ignoreWorktreeAdd() worktreeAddVerdict {
	return worktreeAddVerdict{decision: worktreeAddIgnore}
}

func denyWorktreeAdd(reason string) worktreeAddVerdict {
	return worktreeAddVerdict{decision: worktreeAddDeny, reason: reason}
}

// worktreeAddHookOutput は pre-tool-use payload を読み、単独の git worktree add を wx new へ写す。
// 候補を同定できない入力は fail-open で通し、候補を 1 つでも見つけた後は fail-closed で deny に倒す。
// hook は迂回できる UX であって強制境界ではないため、誤った deny の代償のほうが素通しより大きい。
// 第 2 戻り値が false のときは何も出力せず、従来どおり readiness 待ちだけで抜ける。
// commentlint:allow-long -- fail-open と fail-closed を切り替える根拠を残す
func worktreeAddHookOutput(payload []byte, workspaceRoot string) (preToolUseHookOutput, bool) {
	// 全ツール呼び出しで走るため、関係しない payload は JSON を解く前に落とす。
	if !strings.Contains(string(payload), "worktree") {
		return preToolUseHookOutput{}, false
	}
	var decoded preToolUsePayload
	if json.Unmarshal(payload, &decoded) != nil {
		return preToolUseHookOutput{}, false
	}
	raw, exists := decoded.ToolInput["command"]
	if !exists {
		return preToolUseHookOutput{}, false
	}
	var command string
	if json.Unmarshal(raw, &command) != nil {
		return preToolUseHookOutput{}, false
	}
	if !commandRunsInWorkspace(decoded.CWD, workspaceRoot) {
		return preToolUseHookOutput{}, false
	}
	verdict := classifyWorktreeAddCommand(command, worktreeAddLocation{cwd: decoded.CWD, root: workspaceRoot})
	switch verdict.decision {
	case worktreeAddRewrite:
		updated := make(map[string]json.RawMessage, len(decoded.ToolInput))
		maps.Copy(updated, decoded.ToolInput)
		encoded, err := json.Marshal(verdict.command)
		if err != nil {
			return preToolUseHookOutput{}, false
		}
		updated["command"] = encoded
		guidance := rewriteContext
		if verdict.compound {
			guidance = compoundRewriteContext
		}
		return preToolUseHookOutput{
			HookSpecificOutput: preToolUseHookSpecificOutput{
				HookEventName:            "PreToolUse",
				PermissionDecision:       "allow",
				PermissionDecisionReason: rewriteReason,
				AdditionalContext:        guidance,
				UpdatedInput:             updated,
			},
			SystemMessage: rewriteSystemMessage,
		}, true
	case worktreeAddDeny:
		return denyPreToolUse(verdict.reason+" "+denyGuidance[verdict.reason], denySystemMessage), true
	case worktreeAddIgnore:
		return preToolUseHookOutput{}, false
	}
	return preToolUseHookOutput{}, false
}

// writeUnmanagedPreToolUseDecision は wx 外の session の判定を 1 つだけ stdout へ書く。
// pre-tool-use の stdout は JSON として解釈されるため、判定が無いときは何も書かない。
// directRoot は wx -n の直接起動でだけ渡る書き換えの境界で、git worktree add の書き換えを優先する。
func writeUnmanagedPreToolUseDecision(ctx context.Context, payload []byte, directRoot string) {
	if directRoot != "" {
		if output, ok := preToolUseDecision(ctx, payload, directRoot, false); ok {
			writePreToolUseOutput(output)
			return
		}
	}
	if output, ok := unmanagedSubagentIsolationHookOutput(ctx, payload, configuredWorktreeMode); ok {
		writePreToolUseOutput(output)
	}
}

// writePreToolUseOutput は判定済みの 1 つの JSON を stdout へ書く。
func writePreToolUseOutput(output preToolUseHookOutput) {
	encoded, err := json.Marshal(output)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, string(encoded))
}

// preToolUseDecision は git worktree add の書き換えを優先し、判定を 1 つだけ選ぶ。
// ここでの SubAgent の isolation とブランチ attach の deny は管理下 session（WX_SESSION_ID あり）に限る。
// wx 外の session の isolation は writeUnmanagedPreToolUseDecision が保存済み方針を引いて判定する。
// 書き換えは allow で返り連結の残りも判定を通らないため、管理下では書き換え後の command をブランチ attach の判定に通す。
// commentlint:allow-long -- 書き換え後にも attach の判定を通す理由を残す
func preToolUseDecision(ctx context.Context, payload []byte, workspaceRoot string, managed bool) (preToolUseHookOutput, bool) {
	if len(payload) == 0 {
		return preToolUseHookOutput{}, false
	}
	if output, ok := worktreeAddHookOutput(payload, workspaceRoot); ok || !managed {
		if ok && managed && output.HookSpecificOutput.UpdatedInput != nil {
			if denied, deny := branchAttachHookOutput(ctx, rewrittenPayload(payload, output.HookSpecificOutput.UpdatedInput)); deny {
				return denied, true
			}
		}
		return output, ok
	}
	if output, ok := subagentIsolationHookOutput(payload); ok {
		return output, true
	}
	return branchAttachHookOutput(ctx, payload)
}

// rewrittenPayload は payload の tool_input を書き換え後の入力へ差し替える。組み立てに失敗すれば元の payload を返す。
func rewrittenPayload(payload []byte, updated map[string]json.RawMessage) []byte {
	var decoded map[string]json.RawMessage
	if json.Unmarshal(payload, &decoded) != nil {
		return payload
	}
	toolInput, err := json.Marshal(updated)
	if err != nil {
		return payload
	}
	decoded["tool_input"] = toolInput
	rebuilt, err := json.Marshal(decoded)
	if err != nil {
		return payload
	}
	return rebuilt
}

// commandRunsInWorkspace は cwd がこの session へ貸し出した workspace の配下かを判定する。
// 別の session の workspace で走る呼び出しを書き換えないための判定で、配下の入れ子 clone は区別しない。
// どの repository への git worktree add かはコマンド文字列からは決まらないためである。
func commandRunsInWorkspace(cwd, workspaceRoot string) bool {
	// cwd を載せない agent があるため、cwd が無いことだけを理由に判定を諦めない。
	if cwd == "" {
		return true
	}
	if workspaceRoot == "" {
		return false
	}
	root := filepath.Clean(workspaceRoot)
	target := filepath.Clean(cwd)
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

// classifyWorktreeAddCommand は Bash の command 文字列を静的に読み、3 通りの判定を返す。
// 判定はコマンド文字列だけで行い、git -C があるときだけ location を使ってファイルシステムを見る。
func classifyWorktreeAddCommand(command string, location worktreeAddLocation) worktreeAddVerdict {
	segments, wellFormed := lexShellCommand(command)
	candidates := 0
	var chosen worktreeAddCandidate
	for _, segment := range segments {
		candidate, ok := gitWorktreeAddArguments(segment)
		if !ok {
			continue
		}
		candidates++
		chosen = candidate
	}
	if candidates == 0 {
		// 候補が見つからないときだけ、深さ 1 の `sh -c "..."` を拾う。eval や変数経由は塞がない。
		if nestedWorktreeAdd(segments) {
			return denyWorktreeAdd(denyReasonCompound)
		}
		return ignoreWorktreeAdd()
	}
	switch {
	case !wellFormed:
		return denyWorktreeAdd(denyReasonUnsupported)
	case candidates > 1 || len(segments) > 1:
		if command, ok := rewriteCompoundWorktreeAdd(command, location); ok {
			return worktreeAddVerdict{decision: worktreeAddRewrite, command: command, compound: true}
		}
		return denyWorktreeAdd(denyReasonCompound)
	}
	verdict := rewriteWorktreeAddCandidate(chosen, location)
	if verdict.decision == worktreeAddRewrite && verdict.directory != "" {
		// サブシェルにするのは、cd を次のツール呼び出しの cwd へ持ち越させないためである。
		verdict.command = "(cd " + shellSingleQuote(verdict.directory) + " && " + verdict.command + ")"
	}
	return verdict
}

// rewriteWorktreeAddCandidate は git worktree add 1 つを、location を cwd とする wx new の呼び出しへ写す。
// git -C があれば directory に実行先を入れ、command は cd を含まない wx new だけにする。
func rewriteWorktreeAddCandidate(candidate worktreeAddCandidate, location worktreeAddLocation) worktreeAddVerdict {
	if candidate.issue != "" {
		return denyWorktreeAdd(candidate.issue)
	}
	verdict := rewriteWorktreeAddArguments(candidate.arguments)
	if verdict.decision != worktreeAddRewrite {
		return verdict
	}
	directory, issue := location.repositoryDirectory(candidate.directories)
	if issue != "" {
		return denyWorktreeAdd(issue)
	}
	verdict.directory = directory
	return verdict
}

// worktreeAddDroppableFlags は wx new へ写すときに落としてよい git worktree add のフラグである。
// 未知の `-*` は deny に倒し、将来の git が足したフラグを黙って落とさない。
var worktreeAddDroppableFlags = map[string]bool{
	"-f": true, "--force": true, "--detach": true, "--checkout": true, "--no-checkout": true,
	"-q": true, "--quiet": true, "--no-track": true, "--guess-remote": true, "--no-guess-remote": true,
	"--relative-paths": true, "--no-relative-paths": true,
}

var worktreeAddBranchFlags = map[string]bool{"-b": true, "-B": true, "--orphan": true}

// rewriteWorktreeAddArguments は `add` 以降の引数を読み、wx new のコマンド行を組み立てる。
// パスは捨てるので展開を含んでよいが、起点とフラグに展開があれば値を決められないため deny に倒す。
// 引用されていない展開が空や複数語になると git 側では位置引数がずれるが、書かれた位置どおりに読む。
// commentlint:allow-long -- 展開を許す位置と、引用なしの展開を許す代償を残す
func rewriteWorktreeAddArguments(arguments []shellWord) worktreeAddVerdict {
	var positional []shellWord
	positionalOnly := false
	for _, argument := range arguments {
		value := argument.value
		switch {
		case positionalOnly || !strings.HasPrefix(value, "-") || value == "-":
			positional = append(positional, argument)
			continue
		case argument.expanded:
			return denyWorktreeAdd(denyReasonUnsupported)
		case value == "--":
			positionalOnly = true
			continue
		case value == "-h" || value == "--help":
			// usage は git 自身に出させる。worktree は作られない。
			return ignoreWorktreeAdd()
		}
		name := value
		if index := strings.Index(name, "="); index > 0 {
			name = name[:index]
		}
		switch {
		case worktreeAddDroppableFlags[name]:
		case worktreeAddBranchFlags[name]:
			return denyWorktreeAdd(denyReasonBranchCreation)
		default:
			return denyWorktreeAdd(denyReasonUnsupported)
		}
	}
	if len(positional) == 0 || len(positional) > 2 {
		return denyWorktreeAdd(denyReasonUnsupported)
	}
	startPoint := ""
	if len(positional) == 2 {
		if positional[1].expanded {
			return denyWorktreeAdd(denyReasonExpandedStartPoint)
		}
		startPoint = positional[1].value
	}
	command, ok := wxNewCommand(startPoint)
	if !ok {
		return denyWorktreeAdd(denyReasonStartPoint)
	}
	return worktreeAddVerdict{decision: worktreeAddRewrite, command: command, path: positional[0]}
}

// wxNewCommand は git worktree add の起点を wx new の呼び出しへ写す。
// 16 進だけの起点は object 名として --commit へ写す。同名の branch があれば wx new 側が拒否する。
// 受理する文字を 16 進と下の branch 名に限るので、組み立てたコマンド行に shell の引用が要らない。
func wxNewCommand(startPoint string) (string, bool) {
	if startPoint == "" || startPoint == "HEAD" {
		return "wx new", true
	}
	if gitx.IsHexObjectName(startPoint) {
		return "wx new --commit " + startPoint, true
	}
	branch := strings.TrimPrefix(startPoint, "origin/")
	if !isPlainBranchName(branch) {
		return "", false
	}
	return "wx new --branch " + branch, true
}

// isPlainBranchName は wx new --branch へそのまま渡せる名前かを判定する。
// 値は internal/pool を経て gitx.ResolveRef へ渡り、refs/heads と refs/remotes/origin しか引かない。
// SHA・tag・revision 式は必ず失敗し、`=` は repo=branch セレクタとして解釈されるため受け付けない。
// SHA は呼び出し側が先に --commit へ振り分けるが、`origin/<16 進>` もここで branch として通さない。
// commentlint:allow-long -- 受理条件の由来（ResolveRef と repo セレクタ）を残す
func isPlainBranchName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return false
	}
	if strings.HasPrefix(name, "refs/") || strings.Contains(name, "..") || gitx.IsHexObjectName(name) {
		return false
	}
	for _, char := range []byte(name) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
		case char == '.' || char == '_' || char == '-' || char == '/' || char == '+':
		default:
			return false
		}
	}
	return true
}

// gitRepositoryOptions は git 自身に別リポジトリを見せる大域オプションである。
// -C だけは worktreeAddCandidate.directories へ取り出し、workspace 内の repository を指すかを別に判定する。
var gitRepositoryOptions = map[string]bool{
	"--git-dir": true, "--work-tree": true, "--namespace": true,
}

// gitGlobalOptionsWithValue は次の語を値として取る大域オプションである。
var gitGlobalOptionsWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--exec-path": true,
}

// worktreeAddCandidate は git worktree add と同定した segment の中身である。
// arguments は `add` 以降の引数、directories は git -C の値で、issue は候補ではあるが書き換えられない理由である。
type worktreeAddCandidate struct {
	arguments, directories []shellWord
	issue                  string
}

// gitWorktreeAddArguments は segment が git worktree add かを判定し、`add` 以降の引数を返す。
// argv[0] が git であることを必ず条件にするので、`echo "git worktree add"` や grep は通過する。
// issue が空でなければ呼び出し側が deny に使う。
func gitWorktreeAddArguments(segment []shellWord) (candidate worktreeAddCandidate, ok bool) {
	index := 0
	issue := ""
	// 先頭の `NAME=value` は環境変数の指定で、git の参照先を差し替え得る。
	for index < len(segment) && isAssignmentWord(segment[index].value) {
		index++
		issue = denyReasonOtherRepository
	}
	if index >= len(segment) || filepath.Base(segment[index].value) != "git" {
		return worktreeAddCandidate{}, false
	}
	index++
	var directories []shellWord
	for index < len(segment) && strings.HasPrefix(segment[index].value, "-") {
		name := segment[index].value
		if name == "-C" && index+1 < len(segment) {
			directories = append(directories, segment[index+1])
			index += 2
			continue
		}
		if position := strings.Index(name, "="); position > 0 {
			name = name[:position]
		} else if gitGlobalOptionsWithValue[name] {
			index++
		}
		if issue == "" {
			issue = denyReasonUnsupported
			if gitRepositoryOptions[name] {
				issue = denyReasonOtherRepository
			}
		}
		index++
	}
	if index+1 >= len(segment) || segment[index].value != "worktree" || segment[index+1].value != "add" {
		return worktreeAddCandidate{}, false
	}
	return worktreeAddCandidate{arguments: segment[index+2:], directories: directories, issue: issue}, true
}

func isAssignmentWord(word string) bool {
	index := strings.Index(word, "=")
	if index <= 0 {
		return false
	}
	for _, char := range []byte(word[:index]) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '_':
		default:
			return false
		}
	}
	return true
}

var nestedShellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// nestedWorktreeAdd は `sh -c "git worktree add ..."` を深さ 1 だけ見る。
func nestedWorktreeAdd(segments [][]shellWord) bool {
	for _, segment := range segments {
		if len(segment) == 0 || !nestedShellNames[filepath.Base(segment[0].value)] {
			continue
		}
		for index := 1; index+1 < len(segment); index++ {
			if segment[index].value != "-c" {
				continue
			}
			inner, _ := lexShellCommand(segment[index+1].value)
			for _, innerSegment := range inner {
				if _, ok := gitWorktreeAddArguments(innerSegment); ok {
					return true
				}
			}
		}
	}
	return false
}

// shellWord は 1 つの語と、その語が受ける shell の解釈を持つ。
// expanded は変数展開・コマンド置換を含むこと、quoted は引用か `\` を含むことを表す。
// globbed は引用されていない `*` `?` `[` を含むこと、tilde は引用されていない `~` で始まることを表す。
// 後ろの 3 つは git -C の値を resolvePolicyDirectory で解決するときにだけ使う。
// commentlint:allow-long -- 各フラグの意味と、使う場面を残す
type shellWord struct {
	value                            string
	expanded, quoted, globbed, tilde bool
}

// shellSeparators は segment を切る shell の演算子とリダイレクトである。
const shellSeparators = ";&|\n\r()<>"

// lexShellCommand は command を shell の演算子で segment へ分け、各 segment を語へ分解する。
// 引用が閉じない場合も途中までの語を返して ok=false とし、候補を見つけた側が deny に倒せるようにする。
// 厳密な shell の解釈ではなく、単一の simple command かどうかを見分けるのに足りる近似である。
// 最初の `<<` で読むのをやめ、heredoc の本文を語彙解析へ入れない。
// 終端語を同定しないのは、引用形・`<<-`・複数 heredoc・未終端を取り違えると
// 本文の範囲が伸び縮みし、近似の穴が増えるためである。候補が減る方向にしか効かないので、
// 本文の後ろに書いた `git worktree add` は素通しになる（`<<<` も本文とみなす）。
// commentlint:allow-long -- 終端語を見ない選択と、その代償である素通しを残す
func lexShellCommand(command string) (segments [][]shellWord, ok bool) {
	var segment []shellWord
	var word strings.Builder
	var quote byte
	started, escaped := false, false
	var current shellWord
	wellFormed := true
	flushWord := func() {
		if started {
			current.value = word.String()
			segment = append(segment, current)
			word.Reset()
			started, current = false, shellWord{}
		}
	}
	flushSegment := func() {
		flushWord()
		if len(segment) > 0 {
			segments = append(segments, segment)
			segment = nil
		}
	}
scan:
	for index := 0; index < len(command); index++ {
		char := command[index]
		switch {
		case escaped:
			word.WriteByte(char)
			escaped, started = false, true
		case quote != 0:
			switch {
			case char == quote:
				quote = 0
				started = true
			case quote == '"' && char == '\\':
				escaped = true
			case quote == '"' && (char == '$' || char == '`'):
				current.expanded, started = true, true
				word.WriteByte(char)
			default:
				word.WriteByte(char)
				started = true
			}
		case char == '\\':
			escaped, started, current.quoted = true, true, true
		case char == '\'' || char == '"':
			quote, started, current.quoted = char, true, true
		case char == ' ' || char == '\t':
			flushWord()
		case char == '<' && index+1 < len(command) && command[index+1] == '<':
			break scan
		case strings.IndexByte(shellSeparators, char) >= 0:
			flushSegment()
		case char == '$' || char == '`':
			current.expanded, started = true, true
			word.WriteByte(char)
		default:
			current.globbed = current.globbed || char == '*' || char == '?' || char == '['
			current.tilde = current.tilde || (char == '~' && !started)
			word.WriteByte(char)
			started = true
		}
	}
	if quote != 0 || escaped {
		wellFormed = false
	}
	flushSegment()
	return segments, wellFormed
}

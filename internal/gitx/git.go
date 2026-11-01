package gitx

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/fdexec"
	"github.com/HappyOnigiri/WorktreeX/internal/logstore"
)

type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Elapsed        time.Duration
}

// ErrorClass は Git の失敗を呼び出し側が安全に分岐するための分類である。
type ErrorClass string

const (
	// ErrorClassExecution は Git の起動・設定・権限・タイムアウトなどの実行障害を示す。
	ErrorClassExecution ErrorClass = "execution"
	// ErrorClassNotRepository は Git が対象をリポジトリでないと確認したことを示す。
	ErrorClassNotRepository ErrorClass = "not_repository"
)

// ErrNotRepository は Git の非リポジトリ判定を表す。
var ErrNotRepository = errors.New("git directory is not a repository")

// IsNotRepository は Git の失敗が非リポジトリ判定かを返す。
func IsNotRepository(err error) bool { return errors.Is(err, ErrNotRepository) }

type Error struct {
	Args      []string
	Result    Result
	FailureID string
	Class     ErrorClass
	cause     error
}

// Error は失敗の要約を返す。ExitCode が負のとき、git は終了ステータスを返しておらず
// 起動失敗か signal 終了なので、数値の代わりに cause（chdir 失敗・PATH 不在・timeout など）を本文へ出す。
// Result.Stderr は載せない。hook が stderr へ出した内容が公開エラーへ漏れないようにするためである。
func (e *Error) Error() string {
	command := "command"
	if len(e.Args) > 0 {
		command = e.Args[0]
	}
	if e.Result.ExitCode < 0 {
		if e.cause == nil {
			return fmt.Sprintf("git %s failed without an exit status (failure %s)", command, e.FailureID)
		}
		return fmt.Sprintf("git %s failed without an exit status: %s (failure %s)", command, e.cause, e.FailureID)
	}
	return fmt.Sprintf("git %s failed with exit %d (failure %s)", command, e.Result.ExitCode, e.FailureID)
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Is(target error) bool {
	return target == ErrNotRepository && e.Class == ErrorClassNotRepository
}

type Runner struct {
	Timeout time.Duration
	// DetailDir は daemon 起動時、worker が Git を実行する前に一度だけ設定する。
	// 以降更新しないため getDetailDir の読み取りと競合しない。
	DetailDir string
	// FDHelper は Git の exec 前に descriptor へ fchdir する wx executable。テストでは helper を差し替えられる。
	FDHelper    string
	timeout     sync.RWMutex
	runAt       sync.RWMutex
	beforeRunAt func([]string)
	locks       KeyedLocks
}

// SetBeforeRunAtHook は descriptor-bound command の構築後、子の開始直前に呼ぶテスト用 barrier を設定する。
// preparation worker 間で同時実行されるため同期する。
func (r *Runner) SetBeforeRunAtHook(hook func([]string)) {
	r.runAt.Lock()
	r.beforeRunAt = hook
	r.runAt.Unlock()
}

func (r *Runner) invokeBeforeRunAt(args []string) {
	r.runAt.RLock()
	hook := r.beforeRunAt
	r.runAt.RUnlock()
	if hook != nil {
		hook(append([]string(nil), args...))
	}
}

func (r *Runner) SetDetailDir(path string) {
	r.DetailDir = path
}

func (r *Runner) getDetailDir() string {
	return r.DetailDir
}

func (r *Runner) SetTimeout(timeout time.Duration) {
	r.timeout.Lock()
	r.Timeout = timeout
	r.timeout.Unlock()
}

func (r *Runner) GetTimeout() time.Duration {
	r.timeout.RLock()
	defer r.timeout.RUnlock()
	return r.Timeout
}

func (r *Runner) Run(ctx context.Context, dir string, args ...string) (Result, error) {
	return r.RunEnvInput(ctx, dir, nil, nil, args...)
}

func (r *Runner) RunEnv(ctx context.Context, dir string, env []string, args ...string) (Result, error) {
	return r.RunEnvInput(ctx, dir, env, nil, args...)
}

func (r *Runner) RunEnvInput(ctx context.Context, dir string, env []string, input []byte, args ...string) (Result, error) {
	return r.runEnvInput(ctx, dir, nil, env, input, args...)
}

// RunAt は dirFile を CWD として Git を実行する。trampoline が exec 前に fchdir するため、
// 起動中に path が rename・置換されても相対引数は pin した inode を参照する。
func (r *Runner) RunAt(ctx context.Context, dirFile *os.File, env []string, input []byte, args ...string) (Result, error) {
	return r.runEnvInput(ctx, string(filepath.Separator), dirFile, env, input, args...)
}

func (r *Runner) runEnvInput(ctx context.Context, dir string, dirFile *os.File, env []string, input []byte, args ...string) (Result, error) {
	if timeout := r.GetTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		var cmd *exec.Cmd
		if dirFile == nil {
			cmd = exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			cmd.Env = append(sanitizedEnviron(), env...)
		} else {
			var err error
			cmd, err = fdexec.Start(ctx, r.FDHelper, dirFile, append(sanitizedEnviron(), env...), append([]string{"git"}, args...)...)
			if err != nil {
				res := Result{ExitCode: -1, Elapsed: time.Since(start)}
				failureID := newFailureID()
				r.writeFailureDetail(failureID, args, res, err)
				return res, &Error{Args: append([]string(nil), args...), Result: res, FailureID: failureID, Class: ErrorClassExecution, cause: err}
			}
			r.invokeBeforeRunAt(args)
		}
		if input != nil {
			cmd.Stdin = bytes.NewReader(input)
		}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		res := Result{Stdout: out.String(), Stderr: stderr.String(), Elapsed: time.Since(start)}
		if err == nil {
			return res, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
		if attempt >= 3 || !isLockConflict(res.Stderr) {
			failureID := newFailureID()
			cause := err
			contextErr := ctx.Err()
			if contextErr != nil {
				cause = contextErr
			}
			// 想定内の終了も error と Result は返し、呼び出し側の判定を維持する。
			if !expectedExit(ctx, res) || contextErr != nil {
				r.writeFailureDetail(failureID, args, res, cause)
			}
			return res, &Error{Args: append([]string(nil), args...), Result: res, FailureID: failureID, Class: classifyError(args, res), cause: cause}
		}
		delay := time.Duration(25*(1<<attempt)) * time.Millisecond
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func classifyError(args []string, result Result) ErrorClass {
	if isNotRepositoryFailure(args, result.Stderr) {
		return ErrorClassNotRepository
	}
	return ErrorClassExecution
}

func isNotRepositoryFailure(args []string, stderr string) bool {
	if len(args) == 0 || args[0] != "rev-parse" {
		return false
	}
	return strings.Contains(strings.ToLower(stderr), "not a git repository")
}

// gitEnvDenylist は daemon から child Git へ渡してはならない repository-scoped 環境変数。
// 継承した GIT_DIR、GIT_WORK_TREE、GIT_INDEX_FILE が別 repository を指すと snapshot が成功しても dirty worktree を未保存で削除し得る。
// archive などが明示した環境変数は sanitization 後に追加し、意図した値を優先する。
var gitEnvDenylist = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,

	"GIT_CEILING_DIRECTORIES": true,
	"GIT_CONFIG_GLOBAL":       true,
	"GIT_CONFIG_SYSTEM":       true,
	"GIT_NAMESPACE":           true,
}

// isGitConfigKeyValueVar は config entry を注入する連番付きの GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> か判定する。
// 連番のため gitEnvDenylist には完全名で列挙できない。
func isGitConfigKeyValueVar(name string) bool {
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// sanitizedEnviron は repository-scoped Git 変数を除いた現在の環境を返す。
// scripts/hook-check.sh の child Git に対する除去処理と同じ目的である。
func sanitizedEnviron() []string {
	environ := os.Environ()
	sanitized := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && (gitEnvDenylist[name] || isGitConfigKeyValueVar(name)) {
			continue
		}
		sanitized = append(sanitized, kv)
	}
	return sanitized
}

// newFailureID は crypto/rand.Text() を使う。
// Go 1.27 の crypto/rand.Read は対応 platform で失敗しないため、到達不能な fallback は持たない。
func newFailureID() string {
	return rand.Text()
}

// DetailPath は detailDir に置かれる詳細ログの path を組む。
// 失敗を記録した側と、後からその出力を読ませる側が同じ規則で path を組むための唯一の場所である。
func DetailPath(detailDir, id string) string {
	if detailDir == "" || id == "" {
		return ""
	}
	return filepath.Join(detailDir, id+".log")
}

// WriteDetail は detailDir へ新しい識別子の詳細ログを書き、書けた path を返す。
// 識別子は Git の失敗が持つ FailureID と同じ体系にし、失敗と非失敗の記録が同じ置き場で混ざらないようにする。
func WriteDetail(detailDir, content string) string {
	return writeDetail(detailDir, newFailureID(), content)
}

// writeDetail は id を指定して詳細ログを書く。
// 診断の保存に失敗しても呼び出し側の結果は変えないため、書けなければ空を返すだけにする。
func writeDetail(detailDir, id, content string) string {
	return logstore.WriteDetail(detailDir, id, content)
}

// FormatCommandDetail は Git 1 回分の実行内容と出力を詳細ログの本文へ整える。
// cause は起動失敗のように stderr が空になる失敗の原因で、nil なら行ごと省く。
// 複数行になり得る stderr は末尾へ置く。
func FormatCommandDetail(args []string, result Result, cause error) string {
	quotedArgs := make([]string, len(args))
	for index, argument := range args {
		quotedArgs[index] = strconv.Quote(argument)
	}
	causeLine := ""
	if cause != nil {
		causeLine = fmt.Sprintf("cause: %s\n", cause)
	}
	return fmt.Sprintf("command: git %s\nexit_status: %d\n%selapsed: %s\nstderr:\n%s", strings.Join(quotedArgs, " "), result.ExitCode, causeLine, result.Elapsed, result.Stderr)
}

func (r *Runner) writeFailureDetail(id string, args []string, result Result, cause error) {
	_ = writeDetail(r.getDetailDir(), id, FormatCommandDetail(args, result, cause))
}

func isLockConflict(stderr string) bool {
	message := strings.ToLower(stderr)
	return strings.Contains(message, "could not lock") || strings.Contains(message, "unable to create") && strings.Contains(message, ".lock") || strings.Contains(message, "another git process")
}

// WorktreeRecord は `git worktree list --porcelain -z` の NUL 区切り出力から解析した一件。
type WorktreeRecord struct {
	Path       string
	Bare       bool
	Locked     bool
	LockReason string
}

// ParseWorktreeRecords は `git worktree list --porcelain -z` の NUL 区切り出力を worktree ごとの record に解析する。
// discovery は main worktree の探索に、workspace は lock 状態の確認に同じ形式を使う。
func ParseWorktreeRecords(output string) []WorktreeRecord {
	var records []WorktreeRecord
	var current *WorktreeRecord
	for _, field := range strings.Split(output, "\x00") {
		switch {
		case strings.HasPrefix(field, "worktree "):
			if current != nil {
				records = append(records, *current)
			}
			current = &WorktreeRecord{Path: strings.TrimPrefix(field, "worktree ")}
		case current == nil:
			continue
		case field == "bare":
			current.Bare = true
		case field == "locked":
			current.Locked = true
			current.LockReason = ""
		case strings.HasPrefix(field, "locked "):
			current.Locked = true
			current.LockReason = strings.TrimPrefix(field, "locked ")
		}
	}
	if current != nil {
		records = append(records, *current)
	}
	return records
}

func ResolveRef(ctx context.Context, r *Runner, repo, branch string) (string, bool, error) {
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		// --quiet は ref 不在を exit 1・stderr 空で返すため、実行障害と区別できる。
		res, err := r.Run(WithExpectedExitCodes(ctx, 1), repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err == nil {
			return strings.TrimSpace(res.Stdout), true, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", false, err
		}
		var ge *Error
		if !errors.As(err, &ge) || ge.Result.ExitCode != 1 || ge.Result.Stderr != "" {
			// context のキャンセル・期限切れや Git の実行障害は、ref 不在に
			// 化けさせず呼び出し元へ返す。
			return "", false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// IsHexObjectName は name が 7〜40 桁の 16 進で、短縮形を含む SHA-1 の object 名として読めるかを返す。
func IsHexObjectName(name string) bool {
	if len(name) < 7 || len(name) > 40 {
		return false
	}
	for _, char := range []byte(name) {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

// ResolveCommit は 16 進の object 名（短縮形を含む）を commit の完全な OID へ解決する。
// 第 2 戻り値が false なのは object が repository に無い場合で、fetch 前の commit がこれに当たる。
// rev-parse は同じ名前の ref を object 名より優先するため、OID が name で始まらない結果は ref を引いたものとして拒否する。
// commentlint:allow-long -- ref 優先の挙動と、それを拒否する理由を残す
func ResolveCommit(ctx context.Context, r *Runner, repo, name string) (string, bool, error) {
	if !IsHexObjectName(name) {
		return "", false, fmt.Errorf("%q is not a hexadecimal object name", name)
	}
	res, err := r.Run(WithExpectedExitCodes(ctx, 1), repo, "rev-parse", "--verify", "--quiet", name+"^{commit}")
	if err == nil {
		oid := strings.TrimSpace(res.Stdout)
		if !strings.HasPrefix(oid, strings.ToLower(name)) {
			return "", false, fmt.Errorf("%s names a ref, not a commit object", name)
		}
		return oid, true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, err
	}
	var ge *Error
	if errors.As(err, &ge) && ge.Result.ExitCode == 1 && ge.Result.Stderr == "" {
		return "", false, nil
	}
	// commit 以外の object や曖昧な短縮形は、不在に化けさせず Git の診断を返す。
	return "", false, err
}

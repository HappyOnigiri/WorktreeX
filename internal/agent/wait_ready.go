package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/rpc"
)

const (
	// userPromptReadinessBudget は user-prompt-submit hook の待機の上限である。
	// Claude Code は 30 秒で hook を cancel し、cancel された hook はプロンプトの処理を止めないため、終了処理の余裕を 1 秒残す。
	userPromptReadinessBudget = 29 * time.Second
	// waitReadyBudget は wx wait-ready の待機の上限である。
	// エージェントの Bash ツールの既定タイムアウト 2 分に収め、打ち切られる前に再実行の案内を返す。
	waitReadyBudget = 100 * time.Second
	// defaultReadinessTimeout は WX_READINESS_TIMEOUT が無いときの readiness timeout である。
	defaultReadinessTimeout = 10 * time.Minute

	// waitReadyCommand は準備中でも pre-tool-use が通すコマンドである。
	waitReadyCommand = "wx wait-ready"
)

const (
	userPromptPendingNotice = "wx readiness notice: wx is still preparing this workspace, so files may be missing and prepare commands may not have run yet. " +
		"Run `" + waitReadyCommand + "` as its own command before any other tool call; other tool calls are blocked until preparation finishes."
	preparingDenyReason = "wx is still preparing this workspace (state=%s), so tool calls are blocked until it is ready. " +
		"Run `" + waitReadyCommand + "` as its own command, then retry this tool call."
	preparingDenySystemMessage = "wx blocked a tool call while the workspace is being prepared; run " + waitReadyCommand + "."
	waitReadyReadyMessage      = "wx: the workspace is ready."
	waitReadyPendingMessage    = "wx: the workspace is still being prepared. Run `" + waitReadyCommand + "` again to keep waiting."
)

// ErrReadinessPending は wx wait-ready が期限内に Full Ready を観測できなかったことを示す。
// 案内は RunWaitReady が出力済みなので、呼び出し側は終了コードだけを決める。
var ErrReadinessPending = errors.New("workspace readiness is still pending")

// RunWaitReady はエージェントのシェルから Full Ready を待つ。
// hook の 30 秒上限を超える準備は、pre-tool-use の deny がこのコマンドへ誘導して待たせる。
func RunWaitReady(ctx context.Context, stdout, stderr io.Writer) error {
	wxID := os.Getenv("WX_SESSION_ID")
	token := os.Getenv("WX_SESSION_TOKEN")
	socket := os.Getenv("WX_DAEMON_SOCKET")
	if wxID == "" || token == "" || socket == "" {
		return errors.New("wx wait-ready runs only inside a wx session: WX_SESSION_ID, WX_SESSION_TOKEN and WX_DAEMON_SOCKET are required")
	}
	budget, err := readinessBudget(waitReadyBudget)
	if err != nil {
		return err
	}
	notice, pending, err := waitReady(ctx, newHookClient(socket), wxID, token, budget)
	if err != nil {
		return err
	}
	if pending {
		_, _ = fmt.Fprintln(stderr, waitReadyPendingMessage)
		return ErrReadinessPending
	}
	_, _ = fmt.Fprintln(stdout, waitReadyReadyMessage)
	if notice != "" {
		_, _ = fmt.Fprintln(stdout, notice)
	}
	return nil
}

// readinessBudget は WX_READINESS_TIMEOUT と呼び出し側の上限の短い方を返す。
// WX_READINESS_TIMEOUT で短くできるのは、テストが実時間の上限を待たずに期限切れを観測するためでもある。
func readinessBudget(ceiling time.Duration) (time.Duration, error) {
	timeout := defaultReadinessTimeout
	if raw := os.Getenv("WX_READINESS_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return 0, errors.New("invalid WX_READINESS_TIMEOUT")
		}
		timeout = d
	}
	return min(timeout, ceiling), nil
}

// waitReady は接続の再試行を含めて budget 以内に Full Ready を待ち、準備失敗の案内を 1 回分受け取る。
// 期限切れはエラーではなく pending=true で返す。
// 案内は stdout がエージェントの context に入る呼び出しだけが要求するので、ここを使うのは user-prompt-submit と wx wait-ready に限る。
func waitReady(ctx context.Context, client rpc.Client, wxID, token string, budget time.Duration) (notice string, pending bool, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	params := map[string]any{"session_id": wxID, "token": token, "timeout_ms": int(budget.Milliseconds()), "notice": true}
	var response struct {
		Notice string `json:"notice"`
	}
	if err := client.Call(waitCtx, "WaitReady", params, &response); err != nil {
		// daemon も同じ期限で待つので、client 側の期限切れと daemon が返す期限切れのどちらが先に届くかは競合する。
		// 外側の ctx が生きている限り、どちらも準備中として扱う。
		if ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || strings.HasSuffix(err.Error(), context.DeadlineExceeded.Error())) {
			return "", true, nil
		}
		return "", false, err
	}
	return response.Notice, false, nil
}

// fullyReady は daemon の waitReadiness と同じ Full Ready の判定である。
// Early Ready の後に準備が失敗した貸出も LEASED へ進むので、ここで通る。
func fullyReady(state string) bool {
	return state == "READY" || state == "LEASED"
}

// isWaitReadyInvocation はツール入力の command が wx wait-ready そのものかを返す。
// 準備中の deny が案内するコマンドだけを通すため、前後の空白以外の違いは認めない。
func isWaitReadyInvocation(payload []byte) bool {
	// 全ツール呼び出しで走るため、関係しない payload は JSON を解く前に落とす。
	if !strings.Contains(string(payload), "wait-ready") {
		return false
	}
	var decoded preToolUsePayload
	if json.Unmarshal(payload, &decoded) != nil {
		return false
	}
	var command string
	if json.Unmarshal(decoded.ToolInput["command"], &command) != nil {
		return false
	}
	return strings.TrimSpace(command) == waitReadyCommand
}

package gitx

import "context"

type expectedExitKey struct{}

// WithExpectedExitCodes はこの context で呼ぶ判定用 Git の想定内終了を指定する。
// error と Result は維持し、stderr が空で指定コードに一致するときだけ詳細ログを省く。
// 実行障害を隠さないよう、対象の Run/RunAt 1 回だけへ渡す。
func WithExpectedExitCodes(ctx context.Context, codes ...int) context.Context {
	return context.WithValue(ctx, expectedExitKey{}, append([]int(nil), codes...))
}

func expectedExit(ctx context.Context, result Result) bool {
	if result.ExitCode <= 0 || result.Stderr != "" {
		return false
	}
	codes, _ := ctx.Value(expectedExitKey{}).([]int)
	for _, code := range codes {
		if code == result.ExitCode {
			return true
		}
	}
	return false
}

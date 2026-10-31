package workspace

import (
	"context"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// PriorityGate は待機用の準備を、利用者が待つ処理の間だけ止めておく関門である。
type PriorityGate interface {
	// Wait は利用者が待つ処理が無くなるまで待つ。context が終わったときだけ error を返す。
	Wait(ctx context.Context) error
}

type priorityGateKey struct{}

// WithPriorityGate は、この ctx で進む準備が工程の切れ目で待つ関門を登録する。
// 登録しない ctx（利用者が待つ準備）では待たない。
func WithPriorityGate(ctx context.Context, gate PriorityGate) context.Context {
	if gate == nil {
		return ctx
	}
	return context.WithValue(ctx, priorityGateKey{}, gate)
}

// yieldToPriority は登録された関門で待つ。
// common directory の排他を保持している区間では待たない。止まった準備が利用者側の Git 管理操作を塞ぐためである。
func yieldToPriority(ctx context.Context) error {
	gate, _ := ctx.Value(priorityGateKey{}).(PriorityGate)
	if gate == nil || gitx.HoldsCommonDirLock(ctx) {
		return nil
	}
	return gate.Wait(ctx)
}

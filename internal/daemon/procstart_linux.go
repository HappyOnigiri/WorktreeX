package daemon

import "time"

// osProcessStartTime は linux では開始時刻を取得しない。
// 呼び出し側は取得不可を生存扱いにするため、従来の kill(pid,0) だけの判定になる。
func osProcessStartTime(int) (time.Time, bool) {
	return time.Time{}, false
}

package daemon

import (
	"time"

	"golang.org/x/sys/unix"
)

// osProcessStartTime は kern.proc.pid からプロセスの開始時刻を読む。
// 存在しない PID や別 PID の情報が返った場合は取得不可として扱う。
func osProcessStartTime(pid int) (time.Time, bool) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || info.Proc.P_pid != int32(pid) || info.Proc.P_starttime.Sec == 0 {
		return time.Time{}, false
	}
	started := info.Proc.P_starttime
	return time.Unix(int64(started.Sec), int64(started.Usec)*1000), true
}

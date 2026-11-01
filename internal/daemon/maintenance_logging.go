package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maintenanceLogInterval = time.Hour
	maintenanceLogKeys     = 2048
)

var (
	failureIdentifier = regexp.MustCompile(`\(failure [A-Z2-7]+\)`)
	recoverySession   = regexp.MustCompile(`refs/wx/recovery/[^/\s"]+/`)
)

type maintenanceLogEntry struct {
	signature  string
	last       time.Time
	suppressed int
}

// stableFailure は実行ごとに変わる診断 ID を除き、同じ故障を別の故障と数えないようにする。
func stableFailure(value string) string {
	return failureIdentifier.ReplaceAllString(value, "(failure)")
}

// maintenanceIssue は継続中の故障を周期的に要約する。診断結果や DB の状態は変更しない。
func (m *Manager) maintenanceIssue(key, signature, text string, level slog.Level, attrs ...any) {
	now := m.currentTime()
	m.mu.Lock()
	if m.maintenanceLogs == nil {
		m.maintenanceLogs = map[string]maintenanceLogEntry{}
	}
	previous, exists := m.maintenanceLogs[key]
	if exists && previous.signature == signature && now.Sub(previous.last) < maintenanceLogInterval {
		previous.suppressed++
		m.maintenanceLogs[key] = previous
		m.mu.Unlock()
		return
	}
	if !exists && len(m.maintenanceLogs) >= maintenanceLogKeys {
		for old := range m.maintenanceLogs {
			delete(m.maintenanceLogs, old)
			break
		}
	}
	m.maintenanceLogs[key] = maintenanceLogEntry{signature: signature, last: now}
	m.mu.Unlock()
	attrs = append(attrs, "suppressed", previous.suppressed)
	m.log.Log(m.ctx, level, text, attrs...)
}

func (m *Manager) resolveMaintenanceIssue(key string) {
	m.mu.Lock()
	previous, exists := m.maintenanceLogs[key]
	delete(m.maintenanceLogs, key)
	m.mu.Unlock()
	if exists {
		m.log.Info("maintenance issue resolved", "target", key, "suppressed", previous.suppressed)
	}
}

type gcLogGroup struct {
	Target string
	Status string
	Reason string
	Count  int
}

// summarizeGC は全件理由と連結 error の二重出力を避け、原因別件数と少数の例だけを出す。
// 戻り値の signature は順序・failure ID に依存せず、手動 GC の結果には影響しない。
func summarizeGC(result GCResult, err error) (string, []gcLogGroup) {
	counts := map[string]int{}
	examples := map[string]gcLogGroup{}
	for _, reason := range result.Reasons {
		target := reason.Target
		if strings.HasPrefix(target, "snapshot ") {
			if _, repository, ok := strings.Cut(strings.TrimPrefix(target, "snapshot "), "/"); ok {
				target = "repository " + repository
			}
		}
		cause := recoverySession.ReplaceAllString(stableFailure(reason.Reason), "refs/wx/recovery/<session>/")
		key := target + "/" + reason.Status + "/" + cause
		counts[key]++
		if _, exists := examples[key]; !exists {
			text := stableFailure(reason.Reason)
			if len(text) > 512 {
				text = text[:512] + "..."
			}
			examples[key] = gcLogGroup{target, reason.Status, text, 0}
		}
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d/%d/%d/%d/%d;", result.Candidates, result.Scheduled, result.Completed, result.Pending, result.Failed)
	groups := make([]gcLogGroup, 0, 3)
	for _, key := range keys {
		_, _ = fmt.Fprintf(hash, "%s:%d;", key, counts[key])
		if len(groups) < 3 {
			group := examples[key]
			group.Count = counts[key]
			groups = append(groups, group)
		}
	}
	if len(keys) == 0 && err != nil {
		_, _ = hash.Write([]byte(stableFailure(err.Error())))
	}
	return hex.EncodeToString(hash.Sum(nil)), groups
}

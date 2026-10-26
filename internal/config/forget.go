package config

import (
	"errors"
	"path/filepath"
	"strings"
)

// PreviewWorkspaceRemoval は実体が消えた workspace も設定キーへ解決し、解除後に保存する設定を用意する。
// 戻り値の bool は個別設定の有無を表し、false の場合は CommitEdit を呼ばない。
func PreviewWorkspaceRemoval(path string) (EditPreview, bool, error) {
	if path == "" {
		return EditPreview{}, false, errors.New("path is empty")
	}
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") && !strings.HasPrefix(path, "$HOME") {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return EditPreview{}, false, err
		}
		path = absolute
	}
	root, err := canonicalPath(path)
	if err != nil {
		return EditPreview{}, false, err
	}
	digest, err := configDigest()
	if err != nil {
		return EditPreview{}, false, err
	}
	raw, err := LoadRaw()
	if err != nil {
		return EditPreview{}, false, err
	}
	after, err := configDigest()
	if err != nil {
		return EditPreview{}, false, err
	}
	if digest != after {
		return EditPreview{}, false, ErrConfigChanged
	}
	removed := false
	for key := range raw.Workspaces {
		canonical, err := canonicalPath(key)
		if err != nil {
			return EditPreview{}, false, err
		}
		if canonical == root {
			delete(raw.Workspaces, key)
			removed = true
		}
	}
	// 未知キーの差し戻しで、削除した workspace が復活しないようにする。
	unknown := make([]unknownEntry, 0, len(raw.unknown))
	for _, entry := range raw.unknown {
		if len(entry.path) >= 2 && entry.path[0] == "workspaces" {
			canonical, err := canonicalPath(entry.path[1])
			if err != nil {
				return EditPreview{}, false, err
			}
			if canonical == root {
				continue
			}
		}
		unknown = append(unknown, entry)
	}
	raw.unknown = unknown
	return EditPreview{Request: EditRequest{Target: root}, raw: raw, digest: digest}, removed, nil
}

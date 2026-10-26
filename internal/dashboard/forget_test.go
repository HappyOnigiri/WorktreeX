package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

func TestForgetChoicesWarnOnlyForMissingPathsAndOfferExplicitDiscard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "gone")
	for _, language := range []string{"en", "ja"} {
		t.Run(language, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.System.Language = language
			cfg.Workspaces = map[string]config.Workspace{existing: {}, gone: {}}
			m := newModel(i18n.WithLanguage(context.Background(), language), Options{Config: cfg, CWD: root})
			m.tab, m.selected = 4, 6
			m.width = 300
			updated, _ := m.Update(key(tea.KeyEnter))
			m = updated.(model)
			warning := m.t("dashboard.path_missing")
			for index, option := range m.choices {
				if option.value == existing && option.warning != "" {
					t.Fatalf("existing path warned: %+v", option)
				}
				if option.value == gone {
					if option.warning != warning {
						t.Fatalf("missing warning: %+v", option)
					}
					m.choice = index
				}
			}
			view := strings.Join(m.choiceView(), "\n")
			if !strings.Contains(view, gone+reset+" "+warn+warning+reset) {
				t.Fatalf("missing yellow inline warning: %q", view)
			}
			m.width = 80
			narrow := m.View().Content
			if !strings.Contains(narrow, warn+warning+reset) {
				t.Fatalf("warning was truncated: %q", narrow)
			}
			for _, line := range strings.Split(narrow, "\n") {
				if xansi.StringWidth(line) > 80 {
					t.Fatalf("line exceeds width: %q", line)
				}
			}
			updated, _ = m.Update(key(tea.KeyEnter))
			m = updated.(model)
			if len(m.choices) != 2 || m.choice != 0 || m.choices[0].value != "" || m.choices[1].value != "--discard-recovery" {
				t.Fatalf("forget modes=%+v", m.choices)
			}
			m.choice = 1
			updated, _ = m.Update(key(tea.KeyEnter))
			m = updated.(model)
			confirm := strings.Join(m.confirmView(), "\n")
			if !strings.Contains(confirm, m.t("dashboard.forget_missing")) || !strings.Contains(confirm, m.t("dashboard.forget_discard_warning")) {
				t.Fatalf("confirmation warnings missing: %s", confirm)
			}
			m.finishPending()
			if want := []string{"forget", gone, "--discard-recovery"}; !reflect.DeepEqual(m.result.Args, want) || m.result.WorkDir != "" {
				t.Fatalf("action=%+v want=%v", m.result, want)
			}
		})
	}
}

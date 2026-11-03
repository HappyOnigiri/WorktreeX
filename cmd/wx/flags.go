package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

type agentFlags struct {
	branches       []string
	pullRequests   []string
	fresh          bool
	worktree       bool
	noWorktree     bool
	selectWorktree bool
	setupCheck     bool
}

func parseAgentPrefix(args []string) (agentFlags, string, []string, error) {
	fs := pflag.NewFlagSet("wx", pflag.ContinueOnError)
	fs.SetInterspersed(false)
	var f agentFlags
	fs.StringArrayVar(&f.pullRequests, "pr", nil, "GitHub PR head (number, repo=number, or URL)")
	fs.StringArrayVar(&f.branches, "branch", nil, "detached base branch")
	fs.BoolVar(&f.fresh, "fresh", false, "continue without recovery state")
	fs.BoolVarP(&f.worktree, "worktree", "w", false, "create a worktree for this invocation")
	fs.BoolVarP(&f.noWorktree, "no-worktree", "n", false, "run in the current directory for this invocation")
	fs.BoolVarP(&f.selectWorktree, "select-worktree", "s", false, "select and save this workspace policy again")
	fs.BoolVar(&f.setupCheck, "setup-check", false, "run the initial worktree setup check even if already recorded")
	if err := fs.Parse(args); err != nil {
		return f, "", nil, err
	}
	if (f.worktree && f.noWorktree) || (f.selectWorktree && (f.worktree || f.noWorktree)) {
		return f, "", nil, errors.New("--worktree, --no-worktree, and --select-worktree are mutually exclusive")
	}
	rest := fs.Args()
	if len(rest) == 0 {
		if f.selectWorktree {
			return f, "", nil, nil
		}
		return f, "", nil, pflag.ErrHelp
	}
	agentArgs, branches, prs, err := extractAgentStartPoints(rest[1:])
	if err != nil {
		return f, "", nil, err
	}
	f.branches = append(f.branches, branches...)
	f.pullRequests = append(f.pullRequests, prs...)
	return f, rest[0], agentArgs, nil
}

// extractAgentStartPoints は agent 名の後ろの wx 固有の起点指定を取り除く。
// -- に到達した後は、エージェントが同名の引数を受け取れるよう変更しない。
func extractAgentStartPoints(args []string) (agentArgs, branches, prs []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(agentArgs, args[i:]...), branches, prs, nil
		}
		name, value, inline := strings.Cut(arg, "=")
		if name != "--branch" && name != "--pr" {
			agentArgs = append(agentArgs, arg)
			continue
		}
		if !inline {
			if i+1 >= len(args) || args[i+1] == "--" || (name == "--pr" && strings.HasPrefix(args[i+1], "--")) {
				return nil, nil, nil, fmt.Errorf("flag needs an argument: %s", name)
			}
			i++
			value = args[i]
		}
		if name == "--pr" {
			prs = append(prs, value)
		} else {
			branches = append(branches, value)
		}
	}
	return agentArgs, branches, prs, nil
}

// finishFlagParse は各サブコマンド共通の --help 契約を適用する。
// ContinueOnError では pflag が help の Usage だけを自動表示するため、他の解析エラーは stderr に表示する。
// done が true の場合、呼び出し側は code を返してよい。
func finishFlagParse(fs *pflag.FlagSet, name string, args []string) (code int, done bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			return 0, true
		}
		// ContinueOnError の pflag はエラーを書き出さないため、Usage と併せて stderr に表示する。
		lang := localizedUsageLanguage()
		fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", err)
		commandUsageLanguage(os.Stderr, name, lang)
		return 2, true
	}
	return 0, false
}

// resumeFlagPrefix は先頭の wx オプションだけを分離し、残りを agent の argv として保つ。
func resumeFlagPrefix(args []string) (wxArgs, agentArgs []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return args[:i], args[i+1:]
		case arg == "--fresh" || strings.HasPrefix(arg, "--fresh=") || strings.HasPrefix(arg, "--branch="):
		case arg == "--branch":
			if i+1 < len(args) {
				i++
			}
		default:
			return args[:i], args[i:]
		}
	}
	return args, nil
}

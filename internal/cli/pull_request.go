package cli

import (
	"fmt"
	"os"
)

// WithPullRequests は今回の新規貸出だけに PR 起点を指定する。保存済み方針は変更しない。
func (c Client) WithPullRequests(specs []string) Client {
	c.pullRequests = append([]string(nil), specs...)
	return c
}

func (c Client) rejectPullRequestOptions() int {
	localizer := cliLocalizer(c)
	fmt.Fprintln(os.Stderr, localizer.Localize("cli.error_prefix", nil), localizer.Localize("cli.pr.conflict", nil))
	return 2
}

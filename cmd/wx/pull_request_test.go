package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestAgentPullRequestFlags(t *testing.T) {
	for _, test := range []struct{ args, prs, branches, passed []string }{
		{args: []string{"--pr", "7", "codex", "--pr=b=8", "exec", "--pr", "https://github.com/o/c/pull/9"}, prs: []string{"7", "b=8", "https://github.com/o/c/pull/9"}, passed: []string{"exec"}},
		{args: []string{"--pr=a=7", "--branch", "b=main", "codex", "--branch=c=main", "--", "--pr", "untouched"}, prs: []string{"a=7"}, branches: []string{"b=main", "c=main"}, passed: []string{"--", "--pr", "untouched"}},
		{args: []string{"codex", "--model", "model", "--pr=7"}, prs: []string{"7"}, passed: []string{"--model", "model"}},
	} {
		f, agent, passed, err := parseAgentPrefix(test.args)
		if err != nil || agent != "codex" || !reflect.DeepEqual(f.pullRequests, test.prs) || !reflect.DeepEqual(f.branches, test.branches) || !reflect.DeepEqual(passed, test.passed) {
			t.Fatalf("args=%v flags=%+v agent=%s passed=%v err=%v", test.args, f, agent, passed, err)
		}
	}
	for _, args := range [][]string{{"--pr"}, {"codex", "--pr"}, {"codex", "--pr", "--"}, {"codex", "--pr", "--branch=main"}} {
		if _, _, _, err := parseAgentPrefix(args); err == nil || !strings.Contains(err.Error(), "argument") {
			t.Errorf("args=%v err=%v", args, err)
		}
	}
}

func TestPullRequestLeaseConflicts(t *testing.T) {
	assertLeaseCommandExits(t, "resume", runResume, []string{"session", "codex", "--pr=7"}, 2)
	assertLeaseCommandExits(t, "new", runNew, []string{"--pr", "7", "--commit", "abcdef1"}, 2)
	assertLeaseCommandExits(t, "shell", runShell, []string{"--pr", "7", "--resume", "session"}, 2)
	assertLeaseCommandExits(t, "run", runRun, []string{"--pr", "7", "--resume", "session", "--", "true"}, 2)
}

package gitx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExpectedExitSuppressesOnlyQuietSpecifiedExit(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s' \"$WX_TEST_STDERR\" >&2\nexit \"$WX_TEST_EXIT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, at := range []bool{false, true} {
		for _, tc := range []struct {
			name, exit, stderr string
			detail             bool
		}{
			{"expected", "1", "", false}, {"unexpected", "2", "", true}, {"stderr", "1", "broken", true},
		} {
			t.Run(tc.name+map[bool]string{true: "At", false: "Path"}[at], func(t *testing.T) {
				t.Setenv("WX_TEST_EXIT", tc.exit)
				t.Setenv("WX_TEST_STDERR", tc.stderr)
				dir, details := t.TempDir(), t.TempDir()
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				r := &Runner{DetailDir: details, FDHelper: executable}
				ctx := WithExpectedExitCodes(context.Background(), 1)
				var runErr error
				if at {
					directory, err := os.Open(dir)
					if err != nil {
						t.Fatal(err)
					}
					defer directory.Close()
					_, runErr = r.RunAt(ctx, directory, nil, nil, "symbolic-ref", "-q", "HEAD")
				} else {
					_, runErr = r.Run(ctx, dir, "symbolic-ref", "-q", "HEAD")
				}
				var gitErr *Error
				if !errors.As(runErr, &gitErr) || gitErr.Result.ExitCode <= 0 {
					t.Fatalf("lost exit result: %v", runErr)
				}
				_, err = os.Stat(DetailPath(details, gitErr.FailureID))
				if tc.detail && err != nil || !tc.detail && !os.IsNotExist(err) {
					t.Fatalf("detail expected=%t err=%v", tc.detail, err)
				}
			})
		}
	}
}

func TestExpectedExitStillRecordsStartFailure(t *testing.T) {
	t.Parallel()
	details := t.TempDir()
	r := &Runner{DetailDir: details}
	_, err := r.Run(WithExpectedExitCodes(context.Background(), -1, 1), filepath.Join(t.TempDir(), "gone"), "symbolic-ref", "-q", "HEAD")
	var gitErr *Error
	if !errors.As(err, &gitErr) || gitErr.Result.ExitCode != -1 {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(DetailPath(details, gitErr.FailureID)); err != nil {
		t.Fatal(err)
	}
}

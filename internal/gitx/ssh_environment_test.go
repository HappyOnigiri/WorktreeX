package gitx

import (
	"reflect"
	"testing"
)

func TestSSHEnvironmentCapturesOnlyCallerOverrides(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -i /keys/company -o IdentitiesOnly=yes")
	t.Setenv("GIT_SSH", "/bin/ssh")
	t.Setenv("GIT_SSH_VARIANT", "ssh")
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("GIT_DIR", "/another/repository")
	t.Setenv("GITHUB_TOKEN", "not-forwarded")
	want := []string{"GIT_SSH_COMMAND=ssh -i /keys/company -o IdentitiesOnly=yes", "GIT_SSH=/bin/ssh", "GIT_SSH_VARIANT=ssh", "SSH_AUTH_SOCK="}
	if got := SSHEnvironment(); !reflect.DeepEqual(got, want) {
		t.Fatalf("environment=%v want %v", got, want)
	}
	if err := ValidateSSHEnvironment(want); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSSHEnvironmentRejectsRepositoryOverrides(t *testing.T) {
	for _, env := range [][]string{
		{"GIT_DIR=/another/repository"},
		{"GIT_CONFIG_COUNT=1"},
		{"GITHUB_TOKEN=not-forwarded"},
		{"SSH_AUTH_SOCK"},
		{"SSH_AUTH_SOCK=one", "SSH_AUTH_SOCK=two"},
		{"GIT_SSH_COMMAND=ssh\x00invalid"},
	} {
		if err := ValidateSSHEnvironment(env); err == nil {
			t.Errorf("accepted environment %v", env)
		}
	}
}

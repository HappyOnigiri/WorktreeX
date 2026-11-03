package gitx

import (
	"fmt"
	"os"
	"strings"
)

var sshEnvironmentNames = []string{"GIT_SSH_COMMAND", "GIT_SSH", "GIT_SSH_VARIANT", "SSH_AUTH_SOCK"}

// SSHEnvironment は起動元が明示した SSH の認証・実行設定だけを返す。
// direnv や ssh-agent の設定を要求ごとに渡し、daemon 全体の環境は変更しない。
func SSHEnvironment() []string {
	var env []string
	for _, name := range sshEnvironmentNames {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// ValidateSSHEnvironment は RPC 経由の環境変数を SSH の設定に限定する。
// GIT_DIR などを RunEnv に渡すと repository の境界を越えるため、任意の変数は受け付けない。
func ValidateSSHEnvironment(env []string) error {
	seen := map[string]bool{}
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		allowed := false
		for _, candidate := range sshEnvironmentNames {
			allowed = allowed || name == candidate
		}
		if !ok || !allowed || seen[name] || strings.ContainsRune(entry, '\x00') {
			return fmt.Errorf("invalid SSH environment variable %q", name)
		}
		seen[name] = true
	}
	return nil
}

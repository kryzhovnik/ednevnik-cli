package credentials

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Keychain struct {
	Account, Service string
	Interactive      bool
}

func (k Keychain) PromptSave(ctx context.Context, username string) error {
	if runtime.GOOS != "darwin" {
		return &Error{"credential_unavailable", errors.New("macOS Keychain is unavailable on this platform")}
	}
	if !k.Interactive || username == "" || k.Service == "" {
		return &Error{"credential_unavailable", errors.New("Keychain save requires an explicit interactive login")}
	}
	cmd := exec.CommandContext(ctx, "security", "add-generic-password", "-U", "-a", username, "-s", k.Service, "-w")
	cmd.WaitDelay = time.Second
	cmd.Env = helperEnvironment()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (k Keychain) Load(ctx context.Context) (Credential, error) {
	if runtime.GOOS != "darwin" || !k.Interactive {
		return Credential{}, &Error{"credential_unavailable", errors.New("noninteractive Keychain lookup is unsupported; use the file or environment provider")}
	}
	if k.Account == "" || k.Service == "" {
		return Credential{}, &Error{"credential_malformed", errors.New("Keychain account and service must be explicit")}
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(lookupCtx, "security", "find-generic-password", "-a", k.Account, "-s", k.Service, "-w")
	cmd.WaitDelay = time.Second
	cmd.Env = helperEnvironment()
	stdout := &boundedBuffer{limit: maxCredentialBytes}
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		if lookupCtx.Err() != nil {
			return Credential{}, lookupCtx.Err()
		}
		return Credential{}, &Error{"credential_unavailable", errors.New("Keychain item is missing, locked, denied, or unavailable")}
	}
	password := strings.TrimSuffix(stdout.String(), "\n")
	if password == "" || stdout.overflow {
		return Credential{}, &Error{"credential_malformed", errors.New("Keychain returned an invalid password")}
	}
	return Credential{k.Account, []byte(password)}, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Buffer.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, errors.New("credential helper output exceeds size limit")
	}
	return b.Buffer.Write(p)
}

func helperEnvironment() []string {
	result := make([]string, 0, len(os.Environ()))
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "USER": true, "LOGNAME": true}
	for _, item := range os.Environ() {
		name := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			name = item[:index]
		}
		if allowed[name] {
			result = append(result, item)
		}
	}
	return result
}

package credentials

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
)

const service = "ednevnik-cli"

type Keychain struct{}

func (Keychain) PromptSave(username string) error {
	if username == "" {
		return errors.New("username is required")
	}
	cmd := exec.Command("security", "add-generic-password", "-U", "-a", username, "-s", service, "-w")
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func (Keychain) Load() (string, string, error) {
	accountCmd := exec.Command("security", "find-generic-password", "-s", service)
	var output bytes.Buffer
	accountCmd.Stdout = &output
	accountCmd.Stderr = &output
	if err := accountCmd.Run(); err != nil {
		return "", "", errors.New("no saved eDnevnik credentials; run `ednevnik login --save`")
	}
	username := parseAccount(output.String())
	if username == "" {
		return "", "", errors.New("saved eDnevnik credential has no account name")
	}
	password, err := exec.Command("security", "find-generic-password", "-a", username, "-s", service, "-w").Output()
	if err != nil {
		return "", "", errors.New("cannot read saved eDnevnik password from Keychain")
	}
	return username, strings.TrimSpace(string(password)), nil
}

func parseAccount(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `"acct"<blob>=`) {
			return strings.Trim(strings.TrimPrefix(line, `"acct"<blob>=`), `"`)
		}
	}
	return ""
}

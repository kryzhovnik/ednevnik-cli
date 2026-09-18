package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentPreservesPasswordBytesAndRejectsPartialInput(t *testing.T) {
	t.Setenv("TEST_USER", "parent")
	t.Setenv("TEST_PASSWORD", "  significant whitespace\n")
	got, err := (Environment{"TEST_USER", "TEST_PASSWORD"}).Load(context.Background())
	if err != nil || string(got.Password) != "  significant whitespace\n" {
		t.Fatalf("credential=%q err=%v", got.Password, err)
	}
	os.Unsetenv("TEST_PASSWORD")
	_, err = (Environment{"TEST_USER", "TEST_PASSWORD"}).Load(context.Background())
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != "credential_malformed" {
		t.Fatalf("error=%v", err)
	}
}

func TestFileRequiresPrivateModeAndBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	body := []byte(`{"schema_version":1,"account":"family","origin":"https://portal.example","username":"parent","password":" tail "}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	provider := File{path, "family", "https://portal.example"}
	if _, err := provider.Load(context.Background()); err == nil {
		t.Fatal("insecure file accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := provider.Load(context.Background())
	if err != nil || string(got.Password) != " tail " {
		t.Fatalf("credential=%q err=%v", got.Password, err)
	}
	provider.Account = "other"
	if _, err := provider.Load(context.Background()); err == nil {
		t.Fatal("wrong account accepted")
	}
}

func TestFileRejectsSymlinkAndOversizedInput(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (File{Path: link, Account: "family", Origin: "https://portal.example"}).Load(context.Background()); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.WriteFile(target, make([]byte, maxCredentialBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (File{Path: target, Account: "family", Origin: "https://portal.example"}).Load(context.Background()); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestKeychainUnattendedFailsBeforeHelper(t *testing.T) {
	_, err := (Keychain{Account: "synthetic", Service: "synthetic"}).Load(context.Background())
	var providerErr *Error
	if !errors.As(err, &providerErr) || providerErr.Code != "credential_unavailable" {
		t.Fatalf("error=%v", err)
	}
}

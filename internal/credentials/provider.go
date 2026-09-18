package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const maxCredentialBytes = 64 << 10

type Credential struct {
	Username string
	Password []byte
}
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

type Provider interface {
	Load(context.Context) (Credential, error)
}

type Cached struct {
	provider   Provider
	once       sync.Once
	credential Credential
	err        error
}

func NewCached(provider Provider) *Cached { return &Cached{provider: provider} }
func (p *Cached) Load(ctx context.Context) (Credential, error) {
	p.once.Do(func() { p.credential, p.err = p.provider.Load(ctx) })
	return p.credential, p.err
}

type Environment struct{ UsernameVar, PasswordVar string }

func (p Environment) Load(ctx context.Context) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	username, hasUsername := os.LookupEnv(p.UsernameVar)
	password, hasPassword := os.LookupEnv(p.PasswordVar)
	if !hasUsername && !hasPassword {
		return Credential{}, &Error{"credential_missing", errors.New("environment credentials are not set")}
	}
	if !hasUsername || !hasPassword || username == "" || password == "" {
		return Credential{}, &Error{"credential_malformed", errors.New("environment username and password must both be non-empty")}
	}
	if len(username)+len(password) > maxCredentialBytes {
		return Credential{}, &Error{"credential_malformed", errors.New("environment credentials exceed the size limit")}
	}
	return Credential{username, []byte(password)}, nil
}

type File struct{ Path, Account, Origin string }
type fileDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Account       string `json:"account"`
	Origin        string `json:"origin"`
	Username      string `json:"username"`
	Password      string `json:"password"`
}

func (p File) Load(ctx context.Context) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if p.Path == "" || !filepath.IsAbs(p.Path) {
		return Credential{}, &Error{"credential_malformed", errors.New("credential file path must be absolute")}
	}
	info, err := os.Lstat(p.Path)
	if err != nil {
		return Credential{}, &Error{"credential_unavailable", errors.New("credential file cannot be inspected")}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Credential{}, &Error{"credential_insecure", errors.New("credential file must be regular and accessible only by its owner")}
	}
	if !ownedByCurrentUser(info) {
		return Credential{}, &Error{"credential_insecure", errors.New("credential file must be owned by the current user")}
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return Credential{}, &Error{"credential_unavailable", errors.New("credential file cannot be opened")}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil || len(b) > maxCredentialBytes {
		return Credential{}, &Error{"credential_malformed", errors.New("credential file cannot be read within the size limit")}
	}
	var doc fileDocument
	if err := json.Unmarshal(b, &doc); err != nil || doc.SchemaVersion != 1 || doc.Account != p.Account || doc.Origin != p.Origin || doc.Username == "" || doc.Password == "" {
		return Credential{}, &Error{"credential_malformed", errors.New("credential file is invalid or belongs to another account/origin")}
	}
	return Credential{doc.Username, []byte(doc.Password)}, nil
}

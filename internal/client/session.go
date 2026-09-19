package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"

	jarengine "github.com/kryzhovnik/ednevnik-cli/internal/client/cookiejar"
	"github.com/kryzhovnik/ednevnik-cli/internal/store"
	"golang.org/x/net/publicsuffix"
)

const sessionSchemaVersion = 1
const maxSessionBytes = 1 << 20

var (
	ErrInvalidSession = errors.New("invalid persisted session state")
	ErrLegacySession  = fmt.Errorf("%w: legacy session lacks required cookie metadata; reset the session and log in again", ErrInvalidSession)
)

type sessionEnvelope struct {
	SchemaVersion int                                   `json:"schema_version"`
	Account       string                                `json:"account"`
	Origin        string                                `json:"origin"`
	Cookies       map[string]map[string]jarengine.Entry `json:"cookies"`
}

func newJar() (*jarengine.Jar, error) {
	return jarengine.New(&jarengine.Options{PublicSuffixList: publicsuffix.List})
}

func loadJar(path string, base *url.URL, account string) (*jarengine.Jar, error) {
	jar, err := newJar()
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return jar, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !sessionOwnedByCurrentUser(info) {
		return nil, fmt.Errorf("%w: session file must be a private regular file owned by the current user", ErrInvalidSession)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSessionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSessionBytes {
		return nil, fmt.Errorf("%w: session file exceeds size limit", ErrInvalidSession)
	}
	if len(b) != 0 && b[0] == '[' {
		return nil, ErrLegacySession
	}
	var envelope sessionEnvelope
	if err := json.Unmarshal(b, &envelope); err != nil {
		return nil, ErrInvalidSession
	}
	if envelope.SchemaVersion == 0 {
		return nil, ErrLegacySession
	}
	if envelope.SchemaVersion != sessionSchemaVersion {
		return nil, fmt.Errorf("%w: unsupported schema version", ErrInvalidSession)
	}
	if envelope.Account != account || envelope.Origin != base.Scheme+"://"+base.Host {
		return nil, fmt.Errorf("%w: account or origin mismatch", ErrInvalidSession)
	}
	if envelope.Cookies == nil {
		return nil, fmt.Errorf("%w: cookie state is missing", ErrInvalidSession)
	}
	host := base.Hostname()
	sequences := make(map[uint64]bool)
	for _, domainCookies := range envelope.Cookies {
		for _, cookie := range domainCookies {
			if cookie.HostOnly && cookie.Domain != host {
				return nil, fmt.Errorf("%w: invalid host-only cookie", ErrInvalidSession)
			}
			if !strings.HasPrefix(cookie.Path, "/") || cookie.SeqNum == math.MaxUint64 || cookie.LastAccess.Before(cookie.Creation) {
				return nil, fmt.Errorf("%w: invalid cookie metadata", ErrInvalidSession)
			}
			if sequences[cookie.SeqNum] {
				return nil, fmt.Errorf("%w: duplicate cookie ordering metadata", ErrInvalidSession)
			}
			sequences[cookie.SeqNum] = true
			if err := (&http.Cookie{Name: cookie.Name, Value: cookie.Value, Quoted: cookie.Quoted, Path: cookie.Path, Domain: cookie.Domain}).Valid(); err != nil {
				return nil, fmt.Errorf("%w: invalid cookie syntax", ErrInvalidSession)
			}
			if suffix := publicsuffix.List.PublicSuffix(cookie.Domain); suffix == cookie.Domain && cookie.Domain != host {
				return nil, fmt.Errorf("%w: public-suffix cookie", ErrInvalidSession)
			}
			if cookie.Domain != host && !strings.HasSuffix(host, "."+cookie.Domain) {
				return nil, fmt.Errorf("%w: cookie outside configured origin", ErrInvalidSession)
			}
		}
	}
	if err := jar.Restore(envelope.Cookies); err != nil {
		return nil, fmt.Errorf("%w: invalid cookie identity", ErrInvalidSession)
	}
	_ = jar.Cookies(base) // Drop cookies whose absolute expiry passed offline.
	return jar, nil
}

func saveJar(path string, jar *jarengine.Jar, base *url.URL, account string) error {
	_ = jar.Cookies(base)
	return store.SaveSnapshot(path, sessionEnvelope{
		SchemaVersion: sessionSchemaVersion,
		Account:       account,
		Origin:        base.Scheme + "://" + base.Host,
		Cookies:       jar.Snapshot(),
	})
}

func resetSession(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ResetSessionFile removes only the local cookie snapshot. Account history,
// retained events, and the durable account binding live in separate files.
func ResetSessionFile(path string) error { return resetSession(path) }

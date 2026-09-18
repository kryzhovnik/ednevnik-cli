package client

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	jarengine "github.com/kryzhovnik/ednevnik/internal/client/cookiejar"
	"github.com/kryzhovnik/ednevnik/internal/store"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSessionRoundTripPreservesCookieSemantics(t *testing.T) {
	base := mustURL(t, "https://portal.example")
	jar, err := newJar()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	jar.SetCookies(mustURL(t, "https://portal.example/nested/page"), []*http.Cookie{
		{Name: "host", Value: "host", Path: "/", Secure: true, HttpOnly: true, Expires: now.Add(time.Hour)},
		{Name: "domain", Value: "domain", Domain: "portal.example", Path: "/nested", Expires: now.Add(2 * time.Hour)},
		{Name: "dup", Value: "root", Path: "/", Expires: now.Add(time.Hour)},
		{Name: "dup", Value: "deep", Path: "/nested", Expires: now.Add(time.Hour)},
		{Name: "quoted", Value: "space value", Quoted: true, Path: "/", Expires: now.Add(time.Hour)},
	})
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveJar(path, jar, base, "family"); err != nil {
		t.Fatal(err)
	}
	restored, err := loadJar(path, base, "family")
	if err != nil {
		t.Fatal(err)
	}
	want, got := jar.Cookies(mustURL(t, "https://portal.example/nested/x")), restored.Cookies(mustURL(t, "https://portal.example/nested/x"))
	if len(got) != len(want) {
		t.Fatalf("cookies=%#v want=%#v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Value != want[i].Value || got[i].Quoted != want[i].Quoted {
			t.Fatalf("cookie[%d]=%#v want=%#v", i, got[i], want[i])
		}
	}
	if cookies := restored.Cookies(mustURL(t, "http://portal.example/nested/x")); cookieNamed(cookies, "host") {
		t.Fatal("Secure cookie sent over HTTP")
	}
	if cookies := restored.Cookies(mustURL(t, "https://sub.portal.example/nested/x")); cookieNamed(cookies, "host") || !cookieNamed(cookies, "domain") {
		t.Fatalf("scope mismatch: %#v", cookies)
	}
}

func TestSessionDeletionExpiryAndBinding(t *testing.T) {
	base := mustURL(t, "https://portal.example")
	jar, _ := newJar()
	jar.SetCookies(base, []*http.Cookie{{Name: "keep", Value: "v", Path: "/", MaxAge: 60}, {Name: "gone", Value: "v", Path: "/", Expires: time.Now().Add(time.Hour)}})
	jar.SetCookies(base, []*http.Cookie{{Name: "gone", Value: "", Path: "/", MaxAge: -1}})
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveJar(path, jar, base, "family"); err != nil {
		t.Fatal(err)
	}
	restored, err := loadJar(path, base, "family")
	if err != nil {
		t.Fatal(err)
	}
	if cookieNamed(restored.Cookies(base), "gone") {
		t.Fatal("deleted cookie resurrected")
	}
	if _, err := loadJar(path, base, "other"); err == nil {
		t.Fatal("wrong account accepted")
	}
	if _, err := loadJar(path, mustURL(t, "https://portal.example:444"), "family"); err == nil {
		t.Fatal("wrong origin accepted")
	}
}

func TestSessionMaxAgeDoesNotRestartAcrossReload(t *testing.T) {
	base := mustURL(t, "https://portal.example")
	jar, _ := newJar()
	jar.SetCookies(base, []*http.Cookie{{Name: "short", Value: "v", Path: "/", MaxAge: 1}})
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveJar(path, jar, base, "family"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	restored, err := loadJar(path, base, "family")
	if err != nil {
		t.Fatal(err)
	}
	if cookieNamed(restored.Cookies(base), "short") {
		t.Fatal("Max-Age restarted across reload")
	}
}

func TestSessionFailedSaveDoesNotReplacePriorSnapshot(t *testing.T) {
	base := mustURL(t, "https://portal.example")
	jar, _ := newJar()
	jar.SetCookies(base, []*http.Cookie{{Name: "one", Value: "old", Path: "/"}})
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := saveJar(path, jar, base, "family"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(base, []*http.Cookie{{Name: "two", Value: "new", Path: "/"}})
	if err := saveJar(path, jar, base, "family"); err == nil {
		t.Fatal("injected save failure succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed save replaced prior snapshot")
	}
}

func TestSessionRejectsLegacyCorruptAndPublicSuffixCookie(t *testing.T) {
	base := mustURL(t, "https://school.co.uk")
	jar, _ := newJar()
	jar.SetCookies(base, []*http.Cookie{{Name: "bad", Value: "v", Domain: "co.uk", Path: "/"}})
	if cookieNamed(jar.Cookies(base), "bad") {
		t.Fatal("public-suffix cookie accepted")
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(`[{"name":"legacy"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadJar(path, base, "family"); !errors.Is(err, ErrLegacySession) {
		t.Fatalf("error=%v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadJar(path, base, "family"); err == nil {
		t.Fatal("corrupt session accepted")
	}
}

func TestSessionRejectsInsecureSymlinkAndOversizedFiles(t *testing.T) {
	base := mustURL(t, "https://portal.example")
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "session.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadJar(link, base, "family"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("symlink error=%v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, make([]byte, maxSessionBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadJar(link, base, "family"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("oversized error=%v", err)
	}
}

func TestSessionRejectsForgedPublicSuffixSnapshot(t *testing.T) {
	base := mustURL(t, "https://school.co.uk")
	now := time.Now().UTC()
	envelope := sessionEnvelope{SchemaVersion: 1, Account: "family", Origin: "https://school.co.uk", Cookies: map[string]map[string]jarengine.Entry{
		"co.uk": {"co.uk;/;forged": {Name: "forged", Value: "secret", Domain: "co.uk", Path: "/", Persistent: true, Expires: now.Add(time.Hour), Creation: now, LastAccess: now}},
	}}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := store.SaveSnapshot(path, envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := loadJar(path, base, "family"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("error=%v", err)
	}
}

func cookieNamed(cookies []*http.Cookie, name string) bool {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return true
		}
	}
	return false
}

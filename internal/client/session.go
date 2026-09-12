package client

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

type storedCookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

func loadJar(path string, base *url.URL) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return jar, nil
	}
	if err != nil {
		return nil, err
	}
	var stored []storedCookie
	if err := json.Unmarshal(b, &stored); err != nil {
		return nil, err
	}
	cookies := make([]*http.Cookie, 0, len(stored))
	for _, c := range stored {
		if !c.Expires.IsZero() && time.Now().After(c.Expires) {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, Expires: c.Expires, Secure: c.Secure, HttpOnly: c.HTTPOnly})
	}
	jar.SetCookies(base, cookies)
	return jar, nil
}

func saveJar(path string, jar http.CookieJar, base *url.URL) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cookies := jar.Cookies(base)
	stored := make([]storedCookie, 0, len(cookies))
	for _, c := range cookies {
		stored = append(stored, storedCookie{Name: c.Name, Value: c.Value, Path: c.Path, Domain: c.Domain, Expires: c.Expires, Secure: c.Secure, HTTPOnly: c.HttpOnly})
	}
	b, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

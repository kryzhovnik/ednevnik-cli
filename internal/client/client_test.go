package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/coordination"
)

func TestReadResponseBodyRejectsOversizeWithoutReturningPrefix(t *testing.T) {
	body, err := readResponseBody(bytes.NewReader(make([]byte, maxResponseBytes+1)))
	if !errors.Is(err, ErrResponseTooLarge) || body != nil {
		t.Fatalf("body length=%d err=%v", len(body), err)
	}
}

func testLease(t *testing.T, root, origin string) *coordination.Lease {
	t.Helper()
	config := coordination.DefaultConfig()
	config.RequestPace = 0
	c, err := coordination.New(root, coordination.Namespace{Profile: "test", Origin: origin}, config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}

func TestLoginPersistsSessionForNextClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<form><input name="_token" value="csrf"></form>`)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if r.Form.Get("_token") != "csrf" || r.Form.Get("username") != "user" || r.Form.Get("password") != "secret" {
			http.Error(w, "bad form", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "valid", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<div class="students-list"></div>`) })
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "valid" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, "private")
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	sessionPath := filepath.Join(t.TempDir(), "session.json")
	root := t.TempDir()
	c, err := New(server.URL, sessionPath, "test", testLease(t, root, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background(), "user", "secret"); err != nil {
		t.Fatal(err)
	}
	// A command releases its account lease before the next command starts.
	_ = c.lease.Release()
	c2, err := New(server.URL, sessionPath, "test", testLease(t, root, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	body, err := c2.Get(context.Background(), "/private")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "private" {
		t.Fatalf("body=%q", body)
	}
}

func TestLoginAndReadOverLocalTLS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<input name="_token" value="csrf">`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "tls", Path: "/", Secure: true})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("session"); err != nil || cookie.Value != "tls" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<div class="students-list"></div>`)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	c, err := newClient(server.URL, filepath.Join(t.TempDir(), "session.json"), "test", testLease(t, t.TempDir(), server.URL), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background(), "synthetic", "password"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "/"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedPageAllowsLogoutCSRFToken(t *testing.T) {
	body := []byte(`<div class="students-list"></div><form action="/logout"><input name="_token" value="csrf"></form>`)
	if !authenticatedPage(body) {
		t.Fatal("authenticated logout CSRF form rejected")
	}
}

func TestGetReportsExpiredSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) })
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "login") })
	server := httptest.NewServer(mux)
	defer server.Close()
	c, err := New(server.URL, filepath.Join(t.TempDir(), "session.json"), "test", testLease(t, t.TempDir(), server.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), "/private")
	if err != ErrNotAuthenticated {
		t.Fatalf("got %v, want ErrNotAuthenticated", err)
	}
}

func TestRedirectsAndRetriesEachConsumeBudget(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.URL.Path == "/start" {
				http.Redirect(w, r, "/final", http.StatusFound)
				return
			}
			fmt.Fprint(w, "ok")
		}))
		defer server.Close()
		lease := testLease(t, t.TempDir(), server.URL)
		c, err := New(server.URL, filepath.Join(t.TempDir(), "session.json"), "test", lease)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Get(context.Background(), "/start"); err != nil {
			t.Fatal(err)
		}
		count, _, _, err := lease.Status()
		if err != nil || requests != 2 || count != 2 {
			t.Fatalf("requests=%d count=%d err=%v", requests, count, err)
		}
	})
	t.Run("eligible read retries", func(t *testing.T) {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			http.Error(w, "later", http.StatusInternalServerError)
		}))
		defer server.Close()
		lease := testLease(t, t.TempDir(), server.URL)
		c, err := New(server.URL, filepath.Join(t.TempDir(), "session.json"), "test", lease)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.Get(ctx, "/read")
		count, _, _, err := lease.Status()
		if err != nil || requests != 3 || count != 3 {
			t.Fatalf("requests=%d count=%d err=%v", requests, count, err)
		}
	})
}

func TestClientRejectsForeignTargetsAndRedirectsBeforeForwardingCookies(t *testing.T) {
	var foreignRequests int
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreignRequests++ }))
	defer foreign.Close()
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "secret", Path: "/"})
		http.Redirect(w, r, foreign.URL+"/capture", http.StatusFound)
	}))
	defer portal.Close()
	c, err := New(portal.URL, filepath.Join(t.TempDir(), "session.json"), "test", testLease(t, t.TempDir(), portal.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), foreign.URL+"/direct"); err == nil {
		t.Fatal("foreign initial target accepted")
	}
	if _, err := c.Get(context.Background(), "/redirect"); err == nil {
		t.Fatal("foreign redirect accepted")
	}
	if foreignRequests != 0 {
		t.Fatalf("foreign requests=%d", foreignRequests)
	}
}

func TestLoginRequiresFreshCredentialsAndRecognizedProtectedPage(t *testing.T) {
	var oldCookieForwarded bool
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("old"); err == nil {
			oldCookieForwarded = true
		}
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `<input name="_token" value="csrf">`)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "new", Value: "session", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<h1>maintenance</h1>`) })
	server := httptest.NewServer(mux)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "session.json")
	jar, _ := newJar()
	base := mustURL(t, server.URL)
	jar.SetCookies(base, []*http.Cookie{{Name: "old", Value: "account-a", Path: "/"}})
	if err := saveJar(path, jar, base, "test"); err != nil {
		t.Fatal(err)
	}
	c, err := New(server.URL, path, "test", testLease(t, t.TempDir(), server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background(), "account-b", "password"); err == nil {
		t.Fatal("maintenance page accepted")
	}
	if oldCookieForwarded {
		t.Fatal("old session cookie forwarded during explicit login")
	}
}

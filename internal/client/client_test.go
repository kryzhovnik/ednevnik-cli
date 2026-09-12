package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "home") })
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
	c, err := New(server.URL, sessionPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(context.Background(), "user", "secret"); err != nil {
		t.Fatal(err)
	}
	c2, err := New(server.URL, sessionPath, 0)
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

func TestGetReportsExpiredSession(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/login", http.StatusFound) })
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "login") })
	server := httptest.NewServer(mux)
	defer server.Close()
	c, err := New(server.URL, filepath.Join(t.TempDir(), "session.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(context.Background(), "/private")
	if err != ErrNotAuthenticated {
		t.Fatalf("got %v, want ErrNotAuthenticated", err)
	}
}

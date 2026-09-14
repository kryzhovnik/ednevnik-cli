package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kryzhovnik/ednevnik/internal/client"
	"github.com/kryzhovnik/ednevnik/internal/model"
	"github.com/kryzhovnik/ednevnik/internal/store"
)

type fakeClient struct{ second bool }

func (f *fakeClient) Login(context.Context, string, string) error { return nil }
func (f *fakeClient) BudgetStatus() (string, int, int, error)     { return "2026-09-12", 0, 100, nil }
func (f *fakeClient) Get(_ context.Context, path string) ([]byte, error) {
	if path == "/grades?student=1234567" {
		grades := `<div class="grade numeric">4</div>`
		if f.second {
			grades += `<div class="grade numeric">5</div>`
		}
		return []byte(`<a class="flex-table-row" href="/grades/7654321/show?student=1234567"><div><strong class="d-block">Mathematics</strong><em>Teacher</em></div><div class="grades-cell-wrap">` + grades + `</div></a>`), nil
	}
	if path == "/absents?student=1234567" {
		return []byte(`<div class="categories-wrap"><div class="category-item-wrap green"><span class="category-symbol-subtitle">2. час</span><div class="name">Mathematics</div><div class="name-subtitle">8. септембар 2026.</div></div></div>`), nil
	}
	if path == "/timeline-data?page=1&student=1234567" {
		return []byte(`{"success":true,"meta":{"currentPage":1,"nextPage":null,"lastPage":1},"data":[]}`), nil
	}
	return nil, os.ErrNotExist
}

type retryClient struct {
	gets   int
	logins int
}

func (f *retryClient) Login(context.Context, string, string) error {
	f.logins++
	return nil
}
func (f *retryClient) BudgetStatus() (string, int, int, error) { return "2026-09-12", 0, 100, nil }
func (f *retryClient) Get(context.Context, string) ([]byte, error) {
	f.gets++
	if f.gets == 1 {
		return nil, client.ErrNotAuthenticated
	}
	return []byte("ok"), nil
}

type fakeCredentials struct{}

func (fakeCredentials) PromptSave(string) error       { return nil }
func (fakeCredentials) Load() (string, string, error) { return "user", "password", nil }

func TestGetAutomaticallyLogsInAndRetries(t *testing.T) {
	fake := &retryClient{}
	a := &app{client: fake, creds: fakeCredentials{}}
	body, err := a.get(context.Background(), "/grades")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" || fake.gets != 2 || fake.logins != 1 {
		t.Fatalf("body=%q gets=%d logins=%d", body, fake.gets, fake.logins)
	}
}

func TestSyncWritesSnapshotAndChanges(t *testing.T) {
	fake := &fakeClient{}
	a := &app{client: fake, dir: t.TempDir()}
	if err := a.sync(context.Background(), []string{"--student", "1234567"}); err != nil {
		t.Fatal(err)
	}
	var first model.Snapshot
	if err := store.LoadSnapshot(filepath.Join(a.dir, "latest.json"), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Students) != 1 || len(first.Students[0].Subjects) != 1 || len(first.Students[0].Absences) != 1 {
		t.Fatalf("snapshot=%#v", first)
	}
	fake.second = true
	if err := a.sync(context.Background(), []string{"--force", "--student", "1234567"}); err != nil {
		t.Fatal(err)
	}
	var changes model.Changes
	if err := store.LoadSnapshot(filepath.Join(a.dir, "changes.json"), &changes); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range changes.Items {
		if change.Kind == "subject_grades_changed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("changes=%#v", changes)
	}
}

func TestConsumerUsesIndependentStateDirectory(t *testing.T) {
	a := &app{dir: t.TempDir()}
	defaultDir, err := a.consumerDir("")
	if err != nil || defaultDir != a.dir {
		t.Fatalf("defaultDir=%q err=%v", defaultDir, err)
	}
	pasDir, err := a.consumerDir("pas")
	if err != nil || pasDir != filepath.Join(a.dir, "consumers", "pas") {
		t.Fatalf("pasDir=%q err=%v", pasDir, err)
	}
	if _, err := a.consumerDir("../pas"); err == nil {
		t.Fatal("unsafe consumer name accepted")
	}
}

type timelineClient struct{ paths []string }

func (f *timelineClient) Login(context.Context, string, string) error { return nil }
func (f *timelineClient) BudgetStatus() (string, int, int, error)     { return "2026-09-12", 0, 100, nil }
func (f *timelineClient) Get(_ context.Context, path string) ([]byte, error) {
	f.paths = append(f.paths, path)
	if path == "/timeline-data?page=1&student=1234567" {
		return []byte(`{"success":true,"meta":{"currentPage":1,"nextPage":2,"lastPage":2},"data":[{"date":{"day":"Monday"},"items":[{"id":1,"title":"Math","itemType":"activity"}]}]}`), nil
	}
	if path == "/timeline-data?page=2&student=1234567" {
		return []byte(`{"success":true,"meta":{"currentPage":2,"nextPage":null,"lastPage":2},"data":[{"date":{"day":"Sunday"},"items":[{"id":2,"title":"English","itemType":"activity"}]}]}`), nil
	}
	return nil, os.ErrNotExist
}

func TestTimelineLoadsAllPages(t *testing.T) {
	fake := &timelineClient{}
	a := &app{client: fake, dir: t.TempDir()}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = a.timeline(context.Background(), []string{"--student", "1234567", "--all"})
	w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	var page model.ActivityPage
	if err := json.Unmarshal(buf.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || len(fake.paths) != 2 {
		t.Fatalf("page=%#v paths=%#v", page, fake.paths)
	}
}

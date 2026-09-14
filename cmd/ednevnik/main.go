package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/client"
	"github.com/kryzhovnik/ednevnik/internal/credentials"
	"github.com/kryzhovnik/ednevnik/internal/model"
	"github.com/kryzhovnik/ednevnik/internal/parse"
	"github.com/kryzhovnik/ednevnik/internal/store"
	"golang.org/x/term"
)

const version = "0.1.0-dev"

type app struct {
	client siteClient
	dir    string
	creds  credentialStore
}

type credentialStore interface {
	PromptSave(string) error
	Load() (string, string, error)
}

type siteClient interface {
	Login(context.Context, string, string) error
	Get(context.Context, string) ([]byte, error)
	BudgetStatus() (string, int, int, error)
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(configDir, "ednevnik")
	stateDir := envOr("EDNEVNIK_STATE_DIR", dir)
	baseURL := envOr("EDNEVNIK_BASE_URL", client.DefaultBaseURL)
	c, err := client.New(baseURL, filepath.Join(dir, "session.json"), 2500*time.Millisecond)
	if err != nil {
		return err
	}
	a := &app{client: c, dir: stateDir, creds: credentials.Keychain{}}

	switch args[0] {
	case "login":
		return a.login(ctx, args[1:])
	case "students":
		return a.students(ctx)
	case "subjects":
		return a.subjects(ctx, args[1:])
	case "grades":
		return a.grades(ctx, args[1:])
	case "absences":
		return a.absences(ctx, args[1:])
	case "timeline", "activities":
		return a.timeline(ctx, args[1:])
	case "page":
		return a.page(ctx, args[1:])
	case "sync":
		return a.sync(ctx, args[1:])
	case "changes":
		return a.changes(args[1:])
	case "status":
		return a.status(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a *app) login(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	save := fs.Bool("save", false, "save credentials in macOS Keychain for automatic re-login")
	if err := fs.Parse(args); err != nil {
		return err
	}
	username := os.Getenv("EDNEVNIK_USERNAME")
	password := os.Getenv("EDNEVNIK_PASSWORD")
	reader := bufio.NewReader(os.Stdin)
	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		username, _ = reader.ReadString('\n')
		username = strings.TrimSpace(username)
	}
	if *save && password == "" {
		if a.creds == nil {
			return errors.New("credential storage is unavailable")
		}
		fmt.Fprintln(os.Stderr, "Password will be stored in macOS Keychain.")
		if err := a.creds.PromptSave(username); err != nil {
			return fmt.Errorf("save credentials: %w", err)
		}
		storedUsername, storedPassword, err := a.creds.Load()
		if err != nil {
			return err
		}
		username, password = storedUsername, storedPassword
	} else if password == "" {
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		password = string(b)
	}
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}
	if err := a.client.Login(ctx, username, password); err != nil {
		return err
	}
	if *save && os.Getenv("EDNEVNIK_PASSWORD") != "" {
		return errors.New("cannot save EDNEVNIK_PASSWORD securely; unset it and run interactively")
	}
	fmt.Println("Login successful. Session saved locally with mode 0600.")
	return nil
}

func (a *app) get(ctx context.Context, path string) ([]byte, error) {
	body, err := a.client.Get(ctx, path)
	if !errors.Is(err, client.ErrNotAuthenticated) || a.creds == nil {
		return body, err
	}
	username, password, loadErr := a.creds.Load()
	if loadErr != nil {
		return nil, loadErr
	}
	if loginErr := a.client.Login(ctx, username, password); loginErr != nil {
		return nil, fmt.Errorf("automatic login: %w", loginErr)
	}
	return a.client.Get(ctx, path)
}

func (a *app) students(ctx context.Context) error {
	body, err := a.get(ctx, "/")
	if err != nil {
		return err
	}
	students, err := parse.Students(body, "")
	if err != nil {
		return err
	}
	return output(students)
}

func (a *app) grades(ctx context.Context, args []string) error {
	studentID, err := requiredStudent(args, "grades")
	if err != nil {
		return err
	}
	data, err := a.loadGrades(ctx, studentID)
	if err != nil {
		return err
	}
	return output(data)
}

func (a *app) subjects(ctx context.Context, args []string) error {
	studentID, err := requiredStudent(args, "subjects")
	if err != nil {
		return err
	}
	body, err := a.get(ctx, "/grades?student="+studentID)
	if err != nil {
		return err
	}
	subjects, err := parse.Subjects(body)
	if err != nil {
		return err
	}
	return output(subjects)
}

func (a *app) loadGrades(ctx context.Context, studentID string) (model.StudentData, error) {
	body, err := a.get(ctx, "/grades?student="+studentID)
	if err != nil {
		return model.StudentData{}, err
	}
	subjects, err := parse.Subjects(body)
	if err != nil {
		return model.StudentData{}, err
	}
	data := model.StudentData{Student: model.Student{ID: studentID}, Subjects: subjects, Grades: []model.Grade{}, Absences: []model.Absence{}}
	for _, subject := range subjects {
		body, err = a.get(ctx, "/grades/"+subject.ID+"/show?student="+studentID)
		if err != nil {
			return model.StudentData{}, err
		}
		grades, err := parse.Grades(body, studentID, subject)
		if err != nil {
			return model.StudentData{}, err
		}
		data.Grades = append(data.Grades, grades...)
	}
	return data, nil
}

func (a *app) absences(ctx context.Context, args []string) error {
	studentID, err := requiredStudent(args, "absences")
	if err != nil {
		return err
	}
	body, err := a.get(ctx, "/absents?student="+studentID)
	if err != nil {
		return err
	}
	items, err := parse.Absences(body, studentID)
	if err != nil {
		return err
	}
	return output(items)
}

func (a *app) timeline(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("timeline", flag.ContinueOnError)
	studentID := fs.String("student", "", "student enrolment ID")
	pageNumber := fs.Int("page", 1, "timeline page to fetch")
	allPages := fs.Bool("all", false, "fetch all available timeline pages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateStudentID(*studentID); err != nil {
		return fmt.Errorf("timeline: %w", err)
	}
	if *pageNumber < 1 {
		return errors.New("timeline page must be positive")
	}
	page, err := a.loadTimeline(ctx, *studentID, *pageNumber)
	if err != nil {
		return err
	}
	if *allPages {
		seen := map[int]bool{page.CurrentPage: true}
		for next := page.NextPage; next != nil && *next <= page.LastPage; next = page.NextPage {
			if seen[*next] {
				return errors.New("timeline pagination repeated a page; the site response may have changed")
			}
			seen[*next] = true
			more, err := a.loadTimeline(ctx, *studentID, *next)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, more.Items...)
			page.NextPage = more.NextPage
		}
	}
	return output(page)
}

func (a *app) loadTimeline(ctx context.Context, studentID string, page int) (model.ActivityPage, error) {
	query := url.Values{"student": {studentID}, "page": {strconv.Itoa(page)}}
	body, err := a.get(ctx, "/timeline-data?"+query.Encode())
	if err != nil {
		return model.ActivityPage{}, err
	}
	return parse.Timeline(body, studentID)
}

func (a *app) page(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("page", flag.ContinueOnError)
	path := fs.String("path", "", "site-relative path to fetch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || !strings.HasPrefix(*path, "/") {
		return errors.New("page requires --path beginning with /")
	}
	body, err := a.get(ctx, *path)
	if err != nil {
		return err
	}
	p, err := parse.GenericPage(body, client.DefaultBaseURL)
	if err != nil {
		return err
	}
	return output(p)
}

func (a *app) sync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	var students stringList
	fs.Var(&students, "student", "student enrolment ID; repeat for several students")
	currentOnly := fs.Bool("current", false, "discover and sync every current enrolment")
	force := fs.Bool("force", false, "sync even if the last sync was less than 30 minutes ago")
	consumer := fs.String("consumer", "", "independent snapshot and change stream name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(students) == 0 && !*currentOnly {
		return errors.New("sync requires at least one --student; run `ednevnik students` to list IDs")
	}
	if len(students) > 0 && *currentOnly {
		return errors.New("use either --current or explicit --student values, not both")
	}
	studentInfo := map[string]model.Student{}
	if *currentOnly {
		body, err := a.get(ctx, "/")
		if err != nil {
			return err
		}
		all, err := parse.Students(body, "")
		if err != nil {
			return err
		}
		for _, s := range all {
			if s.Current {
				students = append(students, s.ID)
				studentInfo[s.ID] = s
			}
		}
	}
	for _, id := range students {
		if err := validateStudentID(id); err != nil {
			return err
		}
	}
	stateDir, err := a.consumerDir(*consumer)
	if err != nil {
		return err
	}
	latest := filepath.Join(stateDir, "latest.json")
	var previous model.Snapshot
	_ = store.LoadSnapshot(latest, &previous)
	if !*force && !previous.FetchedAt.IsZero() && time.Since(previous.FetchedAt) < 30*time.Minute {
		return fmt.Errorf("last sync was %s ago; wait 30 minutes or use --force deliberately", time.Since(previous.FetchedAt).Round(time.Second))
	}
	snapshot := model.Snapshot{SchemaVersion: model.SchemaVersion, FetchedAt: time.Now(), Students: []model.StudentData{}}
	for _, studentID := range students {
		current, err := a.loadOverview(ctx, studentID)
		if err != nil {
			return err
		}
		if info, ok := studentInfo[studentID]; ok {
			current.Student = info
		}
		body, err := a.get(ctx, "/absents?student="+studentID)
		if err != nil {
			return err
		}
		current.Absences, err = parse.Absences(body, studentID)
		if err != nil {
			return err
		}
		activities, err := a.loadTimeline(ctx, studentID, 1)
		if err != nil {
			return err
		}
		current.Activities = activities.Items
		snapshot.Students = append(snapshot.Students, current)
	}
	changes := store.Diff(previous, snapshot)
	if err := store.SaveSnapshot(filepath.Join(stateDir, "previous.json"), previous); err != nil {
		return err
	}
	if err := store.SaveSnapshot(latest, snapshot); err != nil {
		return err
	}
	if err := store.SaveSnapshot(filepath.Join(stateDir, "changes.json"), changes); err != nil {
		return err
	}
	if err := appendCheckHistory(filepath.Join(stateDir, "checks.jsonl"), changes); err != nil {
		return err
	}
	return output(snapshot)
}

func (a *app) loadOverview(ctx context.Context, studentID string) (model.StudentData, error) {
	body, err := a.get(ctx, "/grades?student="+studentID)
	if err != nil {
		return model.StudentData{}, err
	}
	subjects, err := parse.Subjects(body)
	if err != nil {
		return model.StudentData{}, err
	}
	return model.StudentData{Student: model.Student{ID: studentID}, Subjects: subjects, Grades: []model.Grade{}, Absences: []model.Absence{}, Activities: []model.Activity{}}, nil
}

func (a *app) changes(args []string) error {
	fs := flag.NewFlagSet("changes", flag.ContinueOnError)
	consumer := fs.String("consumer", "", "independent snapshot and change stream name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stateDir, err := a.consumerDir(*consumer)
	if err != nil {
		return err
	}
	var changes model.Changes
	if err := store.LoadSnapshot(filepath.Join(stateDir, "changes.json"), &changes); err != nil {
		return err
	}
	return output(changes)
}

func (a *app) status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	consumer := fs.String("consumer", "", "independent snapshot and change stream name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	stateDir, err := a.consumerDir(*consumer)
	if err != nil {
		return err
	}
	date, count, limit, budgetErr := a.client.BudgetStatus()
	if budgetErr != nil {
		return budgetErr
	}
	var snapshot model.Snapshot
	err = store.LoadSnapshot(filepath.Join(stateDir, "latest.json"), &snapshot)
	if os.IsNotExist(err) {
		return output(map[string]any{"configured": true, "has_snapshot": false, "request_budget": map[string]any{"date": date, "used": count, "limit": limit}})
	}
	if err != nil {
		return err
	}
	return output(map[string]any{"configured": true, "has_snapshot": true, "last_sync": snapshot.FetchedAt, "students": len(snapshot.Students), "request_budget": map[string]any{"date": date, "used": count, "limit": limit}})
}

func (a *app) consumerDir(consumer string) (string, error) {
	if consumer == "" {
		return a.dir, nil
	}
	for _, r := range consumer {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return "", errors.New("consumer must contain only lowercase letters, digits, and underscores")
		}
	}
	return filepath.Join(a.dir, "consumers", consumer), nil
}

func appendCheckHistory(path string, changes model.Changes) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(map[string]any{
		"checked_at":   changes.ComparedAt,
		"from":         changes.From,
		"to":           changes.To,
		"change_count": len(changes.Items),
	})
}

func requiredStudent(args []string, name string) (string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	student := fs.String("student", "", "student enrolment ID")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *student == "" {
		return "", fmt.Errorf("%s requires --student; run `ednevnik students` to list IDs", name)
	}
	if err := validateStudentID(*student); err != nil {
		return "", err
	}
	return *student, nil
}

func validateStudentID(id string) error {
	if id == "" {
		return errors.New("student ID cannot be empty")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return errors.New("student ID must contain digits only")
		}
	}
	return nil
}

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

func output(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func usage() {
	fmt.Fprintln(os.Stderr, `ednevnik - read structured data from moj.esdnevnik.rs

Usage:
  ednevnik login [--save]
  ednevnik students
  ednevnik subjects --student ID
  ednevnik grades --student ID
  ednevnik absences --student ID
  ednevnik timeline --student ID [--page N | --all]
  ednevnik page --path '/task-schedules?student=ID'
  ednevnik sync --current [--consumer NAME]
  ednevnik sync --student ID --student ID [--consumer NAME]
  ednevnik changes [--consumer NAME]
  ednevnik status [--consumer NAME]

All data commands write JSON to stdout. Snapshots, changes, and timeline pages include a schema version. Diagnostics go to stderr.`)
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/client"
	"github.com/kryzhovnik/ednevnik/internal/model"
	"github.com/kryzhovnik/ednevnik/internal/parse"
	"github.com/kryzhovnik/ednevnik/internal/store"
	"golang.org/x/term"
)

const version = "0.1.0-dev"

type app struct {
	client siteClient
	dir    string
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
	baseURL := envOr("EDNEVNIK_BASE_URL", client.DefaultBaseURL)
	c, err := client.New(baseURL, filepath.Join(dir, "session.json"), 2500*time.Millisecond)
	if err != nil {
		return err
	}
	a := &app{client: c, dir: dir}

	switch args[0] {
	case "login":
		return a.login(ctx)
	case "students":
		return a.students(ctx)
	case "subjects":
		return a.subjects(ctx, args[1:])
	case "grades":
		return a.grades(ctx, args[1:])
	case "absences":
		return a.absences(ctx, args[1:])
	case "page":
		return a.page(ctx, args[1:])
	case "sync":
		return a.sync(ctx, args[1:])
	case "changes":
		return a.changes()
	case "status":
		return a.status()
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

func (a *app) login(ctx context.Context) error {
	username := os.Getenv("EDNEVNIK_USERNAME")
	password := os.Getenv("EDNEVNIK_PASSWORD")
	reader := bufio.NewReader(os.Stdin)
	if username == "" {
		fmt.Fprint(os.Stderr, "Username: ")
		username, _ = reader.ReadString('\n')
		username = strings.TrimSpace(username)
	}
	if password == "" {
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
	fmt.Println("Login successful. Session saved locally with mode 0600.")
	return nil
}

func (a *app) students(ctx context.Context) error {
	body, err := a.client.Get(ctx, "/")
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
	body, err := a.client.Get(ctx, "/grades?student="+studentID)
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
	body, err := a.client.Get(ctx, "/grades?student="+studentID)
	if err != nil {
		return model.StudentData{}, err
	}
	subjects, err := parse.Subjects(body)
	if err != nil {
		return model.StudentData{}, err
	}
	data := model.StudentData{Student: model.Student{ID: studentID}, Subjects: subjects, Grades: []model.Grade{}, Absences: []model.Absence{}}
	for _, subject := range subjects {
		body, err = a.client.Get(ctx, "/grades/"+subject.ID+"/show?student="+studentID)
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
	body, err := a.client.Get(ctx, "/absents?student="+studentID)
	if err != nil {
		return err
	}
	items, err := parse.Absences(body, studentID)
	if err != nil {
		return err
	}
	return output(items)
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
	body, err := a.client.Get(ctx, *path)
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
		body, err := a.client.Get(ctx, "/")
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
	latest := filepath.Join(a.dir, "latest.json")
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
		body, err := a.client.Get(ctx, "/absents?student="+studentID)
		if err != nil {
			return err
		}
		current.Absences, err = parse.Absences(body, studentID)
		if err != nil {
			return err
		}
		snapshot.Students = append(snapshot.Students, current)
	}
	changes := store.Diff(previous, snapshot)
	if err := store.SaveSnapshot(filepath.Join(a.dir, "previous.json"), previous); err != nil {
		return err
	}
	if err := store.SaveSnapshot(latest, snapshot); err != nil {
		return err
	}
	if err := store.SaveSnapshot(filepath.Join(a.dir, "changes.json"), changes); err != nil {
		return err
	}
	return output(snapshot)
}

func (a *app) loadOverview(ctx context.Context, studentID string) (model.StudentData, error) {
	body, err := a.client.Get(ctx, "/grades?student="+studentID)
	if err != nil {
		return model.StudentData{}, err
	}
	subjects, err := parse.Subjects(body)
	if err != nil {
		return model.StudentData{}, err
	}
	return model.StudentData{Student: model.Student{ID: studentID}, Subjects: subjects, Grades: []model.Grade{}, Absences: []model.Absence{}}, nil
}

func (a *app) changes() error {
	var changes model.Changes
	if err := store.LoadSnapshot(filepath.Join(a.dir, "changes.json"), &changes); err != nil {
		return err
	}
	return output(changes)
}

func (a *app) status() error {
	date, count, limit, budgetErr := a.client.BudgetStatus()
	if budgetErr != nil {
		return budgetErr
	}
	var snapshot model.Snapshot
	err := store.LoadSnapshot(filepath.Join(a.dir, "latest.json"), &snapshot)
	if os.IsNotExist(err) {
		return output(map[string]any{"configured": true, "has_snapshot": false, "request_budget": map[string]any{"date": date, "used": count, "limit": limit}})
	}
	if err != nil {
		return err
	}
	return output(map[string]any{"configured": true, "has_snapshot": true, "last_sync": snapshot.FetchedAt, "students": len(snapshot.Students), "request_budget": map[string]any{"date": date, "used": count, "limit": limit}})
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
  ednevnik login
  ednevnik students
  ednevnik subjects --student ID
  ednevnik grades --student ID
  ednevnik absences --student ID
  ednevnik page --path '/task-schedules?student=ID'
  ednevnik sync --current
  ednevnik sync --student ID --student ID
  ednevnik changes
  ednevnik status

All data commands write versioned JSON to stdout. Diagnostics go to stderr.`)
}

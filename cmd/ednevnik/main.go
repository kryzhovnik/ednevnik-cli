package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kryzhovnik/ednevnik/internal/checkstate"
	"github.com/kryzhovnik/ednevnik/internal/client"
	"github.com/kryzhovnik/ednevnik/internal/coordination"
	"github.com/kryzhovnik/ednevnik/internal/credentials"
	"github.com/kryzhovnik/ednevnik/internal/model"
	"github.com/kryzhovnik/ednevnik/internal/parse"
	"github.com/kryzhovnik/ednevnik/internal/store"
	"golang.org/x/term"
)

var version = "0.1.0-dev"

type app struct {
	client      siteClient
	dir         string
	creds       credentials.Provider
	checker     checkRunner
	origin      string
	configDir   string
	lease       *coordination.Lease
	legacyDir   string
	accountDir  string
	profile     string
	authTried   bool
	requireAuth bool
}

var errUnboundLegacyState = errors.New("unbound schema-v2 state requires explicit migration")

type siteClient interface {
	Login(context.Context, string, string) error
	Get(context.Context, string) ([]byte, error)
	BudgetStatus() (string, int, int, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		var silent *exitStatus
		if errors.As(err, &silent) {
			os.Exit(silent.code)
		}
		var structured *commandError
		if errors.As(err, &structured) {
			_ = json.NewEncoder(os.Stderr).Encode(structured.body)
			os.Exit(structured.code)
		}
		if operational := classifyOperationalError(err); operational != nil {
			_ = json.NewEncoder(os.Stderr).Encode(operational.body)
			os.Exit(operational.code)
		}
		if errors.Is(err, parse.ErrInvalidSource) || errors.Is(err, client.ErrResponseTooLarge) {
			failure := newContractError("", model.ReasonInvalidSource, errors.New("portal response did not match the recognized source structure"), false, "Keep the last valid snapshot and inspect portal compatibility before retrying.", 1)
			_ = json.NewEncoder(os.Stderr).Encode(failure.body)
			os.Exit(failure.code)
		}
		if errors.Is(err, errUnboundLegacyState) {
			structured := newContractError("", model.ReasonInvalidState, err, false, "Preserve the files and migrate them explicitly into the selected profile and origin.", 1)
			_ = json.NewEncoder(os.Stderr).Encode(structured.body)
			os.Exit(structured.code)
		}
		fallback := newContractError("", "io", err, true, "Retry after checking local configuration and I/O.", 1)
		_ = json.NewEncoder(os.Stderr).Encode(fallback.body)
		os.Exit(fallback.code)
	}
}

func classifyOperationalError(err error) *commandError {
	var refusal *coordination.Refusal
	if errors.As(err, &refusal) {
		result := newContractError("", model.ReasonRefusalQuota, err, true, "Wait until the reported retry time; force does not bypass request budgets or server cooldowns.", 1)
		result.body.Guidance.RetryAfter = &refusal.RetryAt
		return result
	}
	if errors.Is(err, coordination.ErrInvalidState) {
		return newContractError("", model.ReasonInvalidState, err, false, "Preserve the policy file and repair or migrate it explicitly.", 1)
	}
	if errors.Is(err, coordination.ErrLockTimeout) {
		return newContractError("", model.ReasonConcurrency, err, true, "Retry after the other account command finishes.", 1)
	}
	if errors.Is(err, client.ErrInvalidSession) {
		return newContractError("", model.ReasonInvalidState, err, false, "Run session-reset and perform a fresh explicit login; diary history is preserved.", 1)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newContractError("", model.ReasonCancelled, err, true, "Retry as a new command after the cancellation or deadline condition is resolved.", 1)
	}
	if errors.Is(err, client.ErrOriginMismatch) {
		return newContractError("", "origin_mismatch", err, false, "Use only targets on the configured portal origin.", 1)
	}
	if errors.Is(err, client.ErrAuthRejected) {
		return newContractError("", "auth_rejected", err, false, "Correct the selected credentials; they are not retried automatically.", 1)
	}
	if errors.Is(err, client.ErrAuthInteractionRequired) {
		return newContractError("", "auth_interaction_required", err, false, "Complete the supported password login manually; eID, MFA, CAPTCHA, and browser flows are not automated.", 1)
	}
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode == http.StatusServiceUnavailable {
			retryAt := time.Now().UTC().Add(httpErr.RetryAfter)
			result := newContractError("", model.ReasonRefusalQuota, err, true, "Respect the persisted server cooldown before retrying.", 1)
			result.body.Guidance.RetryAfter = &retryAt
			return result
		}
		if httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden {
			return newContractError("", model.ReasonAuthenticationProvider, err, false, "Authenticate the selected account once; do not retry rejected credentials automatically.", 1)
		}
	}
	var credentialErr *credentials.Error
	if errors.As(err, &credentialErr) {
		return newContractError("", credentialErr.Code, credentialErr, false, "Correct the selected credential provider without falling back to another source.", 1)
	}
	return nil
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return newContractError("", "invalid_argument", flag.ErrHelp, false, "Choose a command shown in help.", 2)
	}
	// Help/version never touch account state.
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	}
	deadline := 5 * time.Minute
	if raw := os.Getenv("EDNEVNIK_COMMAND_TIMEOUT"); raw != "" {
		parsed, parseErr := time.ParseDuration(raw)
		if parseErr != nil || parsed <= 0 {
			return newContractError("", model.ReasonInvalidArgument, errors.New("EDNEVNIK_COMMAND_TIMEOUT must be a positive duration"), false, "Correct the whole-command timeout.", 2)
		}
		deadline = parsed
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, deadline)
	defer cancel()
	configDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(configDir, "ednevnik")
	testLoopback := os.Getenv("EDNEVNIK_TEST_ALLOW_HTTP_LOOPBACK") == "1"
	if testLoopback {
		testRoot := os.Getenv("EDNEVNIK_TEST_STATE_ROOT")
		if !filepath.IsAbs(testRoot) {
			return newContractError("", model.ReasonInvalidArgument, errors.New("loopback test transport requires an absolute isolated state root"), false, "Set EDNEVNIK_TEST_STATE_ROOT to a temporary absolute directory.", 2)
		}
		dir = testRoot
	}
	stateDir := envOr("EDNEVNIK_STATE_DIR", dir)
	if testLoopback {
		stateDir = envOr("EDNEVNIK_TEST_STATE_DIR", dir)
		if !filepath.IsAbs(stateDir) {
			return newContractError("", model.ReasonInvalidArgument, errors.New("loopback test state directory must be absolute"), false, "Set EDNEVNIK_TEST_STATE_DIR to an isolated temporary directory.", 2)
		}
	}
	baseURL := envOr("EDNEVNIK_BASE_URL", client.DefaultBaseURL)
	origin, err := validateConfiguredOrigin(baseURL, testLoopback)
	if err != nil {
		if (args[0] == "status" && !hasProfileFlag(args)) || args[0] == "changes" {
			origin = "local-v2://unbound"
		} else if args[0] == "status" && hasProfileFlag(args) && isLoopbackOrigin(baseURL) {
			// Local recovery may inspect an explicitly named synthetic profile
			// without enabling test transport or making a request.
			origin = canonicalOrigin(baseURL)
		} else {
			return newContractError("", model.ReasonInvalidArgument, errors.New("EDNEVNIK_BASE_URL must be a canonical HTTPS origin without credentials, path, query, or fragment"), false, "Set EDNEVNIK_BASE_URL to the authorized portal HTTPS origin.", 2)
		}
	}
	profile := commandProfile(args)
	if origin == "local-v2://unbound" {
		profile = "legacy"
	}
	if err := validateNamespace(profile, "profile"); err != nil {
		return newContractError("", model.ReasonInvalidArgument, err, false, "Use lowercase letters, digits, and underscores for the profile namespace.", 2)
	}
	policyConfig, err := coordination.ConfigFromEnv()
	if err != nil {
		return newContractError("", model.ReasonInvalidArgument, err, false, "Correct the request-policy configuration.", 2)
	}
	coordinator, err := coordination.New(stateDir, coordination.Namespace{Profile: profile, Origin: origin}, policyConfig)
	if err != nil {
		return err
	}
	lease, err := coordinator.Acquire(ctx)
	if err != nil {
		return classifyCoordinationError(err)
	}
	defer lease.Release()
	if args[0] == "status" || args[0] == "changes" || strings.HasPrefix(args[0], "consumer-") {
		local := &app{dir: stateDir, configDir: dir, origin: origin, lease: lease, legacyDir: filepath.Join(coordinator.StateDir(), "schema-v2"), accountDir: coordinator.StateDir()}
		switch args[0] {
		case "status":
			return local.status(args[1:])
		case "changes":
			return local.changes(args[1:])
		case "consumer-register":
			return local.consumerRegister(args[1:])
		case "consumer-read":
			return local.consumerRead(args[1:])
		case "consumer-ack":
			return local.consumerAck(args[1:])
		}
	}
	sessionPath := filepath.Join(coordinator.StateDir(), "session.json")
	if args[0] == "session-reset" {
		if err := client.ResetSessionFile(sessionPath); err != nil {
			return fmt.Errorf("reset session: %w", err)
		}
		fmt.Println("Local session reset. Diary history and unread events were preserved; remote sessions were not revoked.")
		return nil
	}
	var c *client.Client
	if args[0] == "login" {
		c, err = client.NewFresh(origin, sessionPath, profile, lease)
	} else {
		c, err = client.New(origin, sessionPath, profile, lease)
	}
	if err != nil {
		return err
	}
	a := &app{client: c, dir: stateDir, origin: origin, profile: profile, configDir: dir, lease: lease, legacyDir: filepath.Join(coordinator.StateDir(), "schema-v2"), accountDir: coordinator.StateDir()}
	providerName := os.Getenv("EDNEVNIK_CREDENTIAL_PROVIDER")
	if providerName != "" && args[0] != "login" {
		provider, providerErr := selectedProvider(providerName, profile, origin, testLoopback, false)
		if providerErr != nil {
			return providerErr
		}
		cached := credentials.NewCached(provider)
		selected, loadErr := cached.Load(ctx)
		if loadErr != nil {
			return loadErr
		}
		bound, bindErr := a.validateCredentialAccount(selected.Username)
		if bindErr != nil {
			return bindErr
		}
		a.creds = cached
		a.requireAuth = !bound
	}

	switch args[0] {
	case "login":
		return a.login(ctx, profile, testLoopback, args[1:])
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
	case "check":
		return a.check(ctx, args[1:])
	default:
		return newContractError("", "invalid_argument", fmt.Errorf("unknown command %q", args[0]), false, "Choose a command shown in help.", 2)
	}
}

func isLoopbackOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}

func (a *app) login(ctx context.Context, profile string, testLoopback bool, args []string) error {
	fs := commandFlagSet("login")
	save := fs.Bool("save", false, "save credentials in macOS Keychain for automatic re-login")
	interactive := fs.Bool("interactive", false, "allow terminal prompts for this foreground login")
	providerName := fs.String("provider", os.Getenv("EDNEVNIK_CREDENTIAL_PROVIDER"), "credential provider: env, file, or keychain")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *save {
		return &credentials.Error{Code: "credential_unavailable", Err: errors.New("safe Keychain saving is unsupported; add the exact item in Keychain Access, then use --provider keychain --interactive")}
	}
	if *providerName != "" {
		allowInteractive := *interactive && term.IsTerminal(int(syscall.Stdin))
		provider, err := selectedProvider(*providerName, profile, a.origin, testLoopback, allowInteractive)
		if err != nil {
			return err
		}
		credential, err := provider.Load(ctx)
		if err != nil {
			return err
		}
		if _, err := a.validateCredentialAccount(credential.Username); err != nil {
			return err
		}
		if err := a.loginAndBind(ctx, credential); err != nil {
			return err
		}
		fmt.Println("Login successful. Session saved locally with mode 0600.")
		return nil
	}
	username := ""
	password := ""
	if !*interactive || !term.IsTerminal(int(syscall.Stdin)) {
		return &credentials.Error{Code: "credential_unavailable", Err: errors.New("interactive login requires --interactive and a terminal; select file or env for unattended use")}
	}
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
	if _, err := a.validateCredentialAccount(username); err != nil {
		return err
	}
	if err := a.loginAndBind(ctx, credentials.Credential{Username: username, Password: []byte(password)}); err != nil {
		return err
	}
	fmt.Println("Login successful. Session saved locally with mode 0600.")
	return nil
}

func (a *app) get(ctx context.Context, path string) ([]byte, error) {
	if a.requireAuth && a.creds != nil {
		a.authTried = true
		credential, err := a.creds.Load(ctx)
		if err != nil {
			return nil, err
		}
		if err := a.loginAndBind(ctx, credential); err != nil {
			return nil, fmt.Errorf("automatic login: %w", err)
		}
		a.requireAuth = false
		return a.client.Get(ctx, path)
	}
	body, err := a.client.Get(ctx, path)
	if !errors.Is(err, client.ErrNotAuthenticated) || a.creds == nil {
		return body, err
	}
	if a.authTried {
		return nil, client.ErrNotAuthenticated
	}
	a.authTried = true
	credential, loadErr := a.creds.Load(ctx)
	if loadErr != nil {
		return nil, loadErr
	}
	if _, bindErr := a.validateCredentialAccount(credential.Username); bindErr != nil {
		return nil, bindErr
	}
	if loginErr := a.loginAndBind(ctx, credential); loginErr != nil {
		return nil, fmt.Errorf("automatic login: %w", loginErr)
	}
	return a.client.Get(ctx, path)
}

type accountBinding struct {
	SchemaVersion int    `json:"schema_version"`
	Profile       string `json:"profile"`
	Origin        string `json:"origin"`
	Username      string `json:"username"`
}

func (a *app) validateCredentialAccount(username string) (bool, error) {
	if a.accountDir == "" {
		return true, nil
	}
	path := filepath.Join(a.accountDir, "account.json")
	var existing accountBinding
	err := store.LoadSnapshot(path, &existing)
	if err == nil {
		if existing.SchemaVersion != 1 || existing.Profile != a.profile || existing.Origin != a.origin || existing.Username != username {
			return false, &credentials.Error{Code: "credential_account_mismatch", Err: errors.New("selected credentials do not match the account bound to this profile and origin; use a different profile")}
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("load account binding: %w", err)
	}
	state, stateErr := a.checkStateStore(a.profile).Load(checkProfile{ID: a.profile, Origin: a.origin})
	if stateErr == nil {
		if len(state.Baselines) != 0 || len(state.Events) != 0 || state.LastSuccess != nil {
			return false, &credentials.Error{Code: "credential_account_unbound", Err: errors.New("existing diary history has no credential account binding; use a new profile or migrate it explicitly")}
		}
	} else if !errors.Is(stateErr, checkstate.ErrAbsent) {
		return false, stateErr
	}
	return false, nil
}

func (a *app) commitCredentialAccount(username string) error {
	if a.accountDir == "" {
		return nil
	}
	return store.SaveSnapshot(filepath.Join(a.accountDir, "account.json"), accountBinding{1, a.profile, a.origin, username})
}

func (a *app) loginAndBind(ctx context.Context, credential credentials.Credential) error {
	bound, err := a.validateCredentialAccount(credential.Username)
	if err != nil {
		return err
	}
	if !bound {
		resetter, ok := a.client.(interface{ ResetSession() error })
		if !ok {
			return errors.New("fresh account verification requires session reset support")
		}
		if err := resetter.ResetSession(); err != nil {
			return fmt.Errorf("invalidate obsolete session: %w", err)
		}
	}
	commit := func() error { return a.commitCredentialAccount(credential.Username) }
	if c, ok := a.client.(interface {
		LoginWithCommit(context.Context, string, string, func() error) error
	}); ok {
		return c.LoginWithCommit(ctx, credential.Username, string(credential.Password), commit)
	}
	if err := a.client.Login(ctx, credential.Username, string(credential.Password)); err != nil {
		return err
	}
	return commit()
}

func selectedProvider(name, profile, origin string, testLoopback, interactive bool) (credentials.Provider, error) {
	if testLoopback {
		if os.Getenv("EDNEVNIK_USERNAME") != "" || os.Getenv("EDNEVNIK_PASSWORD") != "" || os.Getenv("EDNEVNIK_CREDENTIALS_FILE") != "" {
			return nil, errors.New("loopback test transport refuses production credential variables")
		}
		switch name {
		case "env":
			if os.Getenv("EDNEVNIK_TEST_CREDENTIALS_FILE") != "" {
				return nil, &credentials.Error{Code: "credential_conflict", Err: errors.New("env and file credential input are both configured")}
			}
			return credentials.Environment{UsernameVar: "EDNEVNIK_TEST_USERNAME", PasswordVar: "EDNEVNIK_TEST_PASSWORD"}, nil
		case "file":
			if os.Getenv("EDNEVNIK_TEST_USERNAME") != "" || os.Getenv("EDNEVNIK_TEST_PASSWORD") != "" {
				return nil, &credentials.Error{Code: "credential_conflict", Err: errors.New("file and env credential input are both configured")}
			}
			return credentials.File{Path: os.Getenv("EDNEVNIK_TEST_CREDENTIALS_FILE"), Account: profile, Origin: origin}, nil
		default:
			return nil, errors.New("loopback test transport supports only synthetic env or file credentials")
		}
	}
	switch name {
	case "env":
		if os.Getenv("EDNEVNIK_CREDENTIALS_FILE") != "" {
			return nil, &credentials.Error{Code: "credential_conflict", Err: errors.New("env and file credential input are both configured")}
		}
		return credentials.Environment{UsernameVar: "EDNEVNIK_USERNAME", PasswordVar: "EDNEVNIK_PASSWORD"}, nil
	case "file":
		if os.Getenv("EDNEVNIK_USERNAME") != "" || os.Getenv("EDNEVNIK_PASSWORD") != "" {
			return nil, &credentials.Error{Code: "credential_conflict", Err: errors.New("file and env credential input are both configured")}
		}
		return credentials.File{Path: os.Getenv("EDNEVNIK_CREDENTIALS_FILE"), Account: profile, Origin: origin}, nil
	case "keychain":
		if !interactive {
			return nil, &credentials.Error{Code: "credential_unavailable", Err: errors.New("unattended Keychain lookup is unsupported; select file or env")}
		}
		return credentials.Keychain{Account: os.Getenv("EDNEVNIK_USERNAME"), Service: keychainService(profile, origin), Interactive: true}, nil
	default:
		return nil, errors.New("credential provider must be one of env, file, or keychain")
	}
}

func keychainService(profile, origin string) string { return "ednevnik-cli:" + profile + ":" + origin }

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
	subjects, err := parse.Subjects(body, studentID)
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
	subjects, err := parse.Subjects(body, studentID)
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
	fs := commandFlagSet("timeline")
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
	return parse.Timeline(body, studentID, page)
}

func (a *app) page(ctx context.Context, args []string) error {
	fs := commandFlagSet("page")
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
	fs := commandFlagSet("sync")
	var students stringList
	fs.Var(&students, "student", "student enrolment ID; repeat for several students")
	profile := fs.String("profile", "", "use the reliable schema-v3 check for this account/profile")
	currentOnly := fs.Bool("current", false, "discover and sync every current enrolment")
	force := fs.Bool("force", false, "bypass only the local minimum check interval")
	consumer := fs.String("consumer", "", "independent snapshot and change stream name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile != "" {
		if *currentOnly || *consumer != "" {
			return errors.New("sync --profile accepts explicit --student values only; --current and --consumer remain legacy schema-v2 options")
		}
		checkArgs := []string{"--profile", *profile}
		if *force {
			checkArgs = append(checkArgs, "--force")
		}
		for _, id := range students {
			checkArgs = append(checkArgs, "--student", id)
		}
		return a.check(ctx, checkArgs)
	}
	if len(students) == 0 && !*currentOnly {
		return errors.New("sync requires at least one --student; run `ednevnik students` to list IDs")
	}
	if len(students) > 0 && *currentOnly {
		return errors.New("use either --current or explicit --student values, not both")
	}
	stateDir, err := a.consumerDir(*consumer)
	if err != nil {
		return err
	}
	latest := filepath.Join(stateDir, "latest.json")
	var previous model.Snapshot
	_ = store.LoadSnapshot(latest, &previous)
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
		if len(students) == 0 {
			return errors.New("no_current_enrolments: recognized family discovery contains no current enrolment")
		}
		discovered := make(map[string]bool, len(students))
		for _, id := range students {
			discovered[id] = true
		}
		for _, old := range previous.Students {
			if old.Student.Current && !discovered[old.Student.ID] {
				return fmt.Errorf("expected current enrolment %s was not discovered", old.Student.ID)
			}
		}
	}
	for _, id := range students {
		if err := validateStudentID(id); err != nil {
			return err
		}
	}
	minimum, err := configuredMinimumCheckInterval()
	if err != nil {
		return err
	}
	if !*force && !previous.FetchedAt.IsZero() && time.Since(previous.FetchedAt) < minimum {
		return fmt.Errorf("last sync was %s ago; wait %s or use --force deliberately", time.Since(previous.FetchedAt).Round(time.Second), minimum)
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
	subjects, err := parse.Subjects(body, studentID)
	if err != nil {
		return model.StudentData{}, err
	}
	return model.StudentData{Student: model.Student{ID: studentID}, Subjects: subjects, Grades: []model.Grade{}, Absences: []model.Absence{}, Activities: []model.Activity{}}, nil
}

func (a *app) changes(args []string) error {
	fs := commandFlagSet("changes")
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

func (a *app) consumerRegister(args []string) error {
	fs := commandFlagSet("consumer-register")
	profile := fs.String("profile", "", "account/profile namespace")
	name := fs.String("consumer", "", "consumer name")
	start := fs.String("start", "", "earliest or latest retained event")
	if err := fs.Parse(args); err != nil {
		return consumerArgumentError(err)
	}
	if *profile == "" || *name == "" || *start == "" {
		return consumerArgumentError(errors.New("consumer-register requires --profile, --consumer, and --start"))
	}
	if err := validateNamespace(*profile, "profile"); err != nil {
		return consumerArgumentError(err)
	}
	if err := validateNamespace(*name, "consumer"); err != nil {
		return consumerArgumentError(err)
	}
	d, err := a.checkStateStore(*profile).Register(checkProfile{ID: *profile, Origin: a.origin}, *name, *start)
	if err != nil {
		return consumerStateError(err)
	}
	return output(map[string]any{"schema_version": checkSchemaVersion, "consumer": *name, "start": *start, "acknowledged_through": d.Consumers[*name].AcknowledgedThrough})
}

func (a *app) consumerRead(args []string) error {
	fs := commandFlagSet("consumer-read")
	profile := fs.String("profile", "", "account/profile namespace")
	name := fs.String("consumer", "", "consumer name")
	limit := fs.Int("limit", 100, "maximum events in the stable batch")
	if err := fs.Parse(args); err != nil {
		return consumerArgumentError(err)
	}
	if *profile == "" || *name == "" {
		return consumerArgumentError(errors.New("consumer-read requires --profile and --consumer"))
	}
	if err := validateNamespace(*profile, "profile"); err != nil {
		return consumerArgumentError(err)
	}
	if err := validateNamespace(*name, "consumer"); err != nil {
		return consumerArgumentError(err)
	}
	b, err := a.checkStateStore(*profile).ReadBatch(checkProfile{ID: *profile, Origin: a.origin}, *name, *limit)
	if err != nil {
		return consumerStateError(err)
	}
	return output(b)
}

func (a *app) consumerAck(args []string) error {
	fs := commandFlagSet("consumer-ack")
	profile := fs.String("profile", "", "account/profile namespace")
	name := fs.String("consumer", "", "consumer name")
	token := fs.String("token", "", "exact token returned by consumer-read")
	if err := fs.Parse(args); err != nil {
		return consumerArgumentError(err)
	}
	if *profile == "" || *name == "" || *token == "" {
		return consumerArgumentError(errors.New("consumer-ack requires --profile, --consumer, and --token"))
	}
	if err := validateNamespace(*profile, "profile"); err != nil {
		return consumerArgumentError(err)
	}
	if err := validateNamespace(*name, "consumer"); err != nil {
		return consumerArgumentError(err)
	}
	d, err := a.checkStateStore(*profile).Acknowledge(checkProfile{ID: *profile, Origin: a.origin}, *name, *token)
	if err != nil {
		return consumerStateError(err)
	}
	return output(map[string]any{"schema_version": checkSchemaVersion, "consumer": *name, "acknowledged": true, "acknowledged_through": d.Consumers[*name].AcknowledgedThrough})
}

func consumerArgumentError(err error) error {
	return newContractError("", model.ReasonInvalidArgument, err, false, "Correct the consumer command arguments.", 2)
}
func consumerStateError(err error) error {
	if errors.Is(err, checkstate.ErrStorageLimit) {
		return newContractError("", model.ReasonStorageLimit, err, false, "Archive the private state and perform an explicit profile reset after accounting for unread events.", 1)
	}
	return newContractError("", model.ReasonInvalidState, err, false, "Preserve the account state and use a registered consumer with its exact delivered token.", 1)
}

func (a *app) status(args []string) error {
	fs := commandFlagSet("status")
	consumer := fs.String("consumer", "", "independent snapshot and change stream name")
	profile := fs.String("profile", "", "schema-v3 account/profile namespace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *profile != "" {
		if *consumer != "" {
			return errors.New("status accepts either --profile or --consumer, not both")
		}
		if err := validateNamespace(*profile, "profile"); err != nil {
			return err
		}
		document, err := a.checkStateStore(*profile).Load(checkProfile{ID: *profile, Origin: a.origin})
		if errors.Is(err, checkstate.ErrAbsent) {
			return output(map[string]any{"schema_version": checkSchemaVersion, "history": "unavailable", "latest_attempt": nil, "last_success": nil})
		}
		if err != nil {
			return newContractError("", model.ReasonInvalidState, err, false, "Preserve the file and repair or migrate local state.", 1)
		}
		return output(checkstate.Status(document))
	}
	stateDir, err := a.consumerDir(*consumer)
	if err != nil {
		return err
	}
	var snapshot model.Snapshot
	err = store.LoadSnapshot(filepath.Join(stateDir, "latest.json"), &snapshot)
	if os.IsNotExist(err) {
		date, count, limit, budgetErr := a.localBudgetStatus()
		if budgetErr != nil {
			return budgetErr
		}
		return output(map[string]any{"configured": true, "has_snapshot": false, "request_budget": map[string]any{"date": date, "used": count, "limit": limit}})
	}
	if err != nil {
		return err
	}
	date, count, limit, budgetErr := a.localBudgetStatus()
	if budgetErr != nil {
		return budgetErr
	}
	return output(map[string]any{"configured": true, "has_snapshot": true, "last_sync": snapshot.FetchedAt, "students": len(snapshot.Students), "request_budget": map[string]any{"date": date, "used": count, "limit": limit}})
}

func (a *app) checkStateStore(profile string) checkstate.Store {
	base := filepath.Join(a.dir, "profiles", profile)
	if a.accountDir != "" {
		base = a.accountDir
	}
	legacy := []string{filepath.Join(a.dir, "latest.json"), filepath.Join(a.dir, "previous.json"), filepath.Join(a.dir, "changes.json"), filepath.Join(a.dir, "checks.jsonl"), filepath.Join(a.dir, "consumers"), filepath.Join(base, "check-status.json")}
	if a.legacyDir != "" {
		legacy = append(legacy, a.legacyDir)
	}
	return checkstate.Store{Path: filepath.Join(base, "check-state.json"), LegacyPaths: legacy}
}

func (a *app) localBudgetStatus() (string, int, int, error) {
	if a.lease == nil {
		return "", 0, 0, errors.New("request policy is unavailable")
	}
	count, limit, _, err := a.lease.Status()
	return time.Now().UTC().Format("2006-01-02"), count, limit, err
}

func (a *app) consumerDir(consumer string) (string, error) {
	base := a.dir
	if a.legacyDir != "" {
		base = a.legacyDir
	}
	if consumer == "" {
		if a.legacyDir != "" && legacyStateExists(a.dir) {
			return "", fmt.Errorf("%w: files exist at %s", errUnboundLegacyState, a.dir)
		}
		return base, nil
	}
	if err := validateNamespace(consumer, "consumer"); err != nil {
		return "", err
	}
	if a.legacyDir != "" && legacyStateExists(filepath.Join(a.dir, "consumers", consumer)) {
		return "", fmt.Errorf("%w: consumer %s", errUnboundLegacyState, consumer)
	}
	return filepath.Join(base, "consumers", consumer), nil
}

func legacyStateExists(dir string) bool {
	for _, name := range []string{"latest.json", "changes.json", "previous.json", "checks.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func validateNamespace(value, name string) error {
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return fmt.Errorf("%s must contain only lowercase letters, digits, and underscores", name)
		}
	}
	return nil
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
	fs := commandFlagSet(name)
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

func commandFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func commandProfile(args []string) string {
	profile := envOr("EDNEVNIK_PROFILE", "default")
	for i := 1; i < len(args); i++ {
		if args[i] == "--profile" && i+1 < len(args) {
			profile = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--profile=") {
			profile = strings.TrimPrefix(args[i], "--profile=")
		}
	}
	return profile
}

func hasProfileFlag(args []string) bool {
	for _, arg := range args[1:] {
		if arg == "--profile" || strings.HasPrefix(arg, "--profile=") {
			return true
		}
	}
	return false
}

func configuredMinimumCheckInterval() (time.Duration, error) {
	raw := envOr("EDNEVNIK_MIN_CHECK_INTERVAL", "30m")
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, errors.New("EDNEVNIK_MIN_CHECK_INTERVAL must be a non-negative duration")
	}
	return d, nil
}

func classifyCoordinationError(err error) error {
	reason, retryable, action := model.ReasonConcurrency, true, "Retry after the other command finishes."
	if errors.Is(err, coordination.ErrBudgetExhausted) || errors.Is(err, coordination.ErrServerCooldown) {
		reason, action = model.ReasonRefusalQuota, "Wait until the reported retry time; force does not bypass request budgets or server cooldowns."
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		reason, retryable, action = model.ReasonCancelled, true, "Retry as a new command when the cancellation or deadline condition is resolved."
	}
	return newContractError("", reason, err, retryable, action, 1)
}

func canonicalOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: strings.ToLower(u.Scheme), Host: strings.ToLower(u.Host)}).String()
}

func validateConfiguredOrigin(raw string, allowTestLoopback bool) (string, error) {
	u, err := url.Parse(raw)
	validScheme := u != nil && u.Scheme == "https"
	if u != nil && allowTestLoopback && u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		validScheme = u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	if err != nil || !validScheme || u.Host == "" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid portal origin")
	}
	return canonicalOrigin(raw), nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `ednevnik - read structured data from moj.esdnevnik.rs

Usage:
  ednevnik login [--provider env|file|keychain] [--interactive] [--save]
  ednevnik session-reset
  ednevnik students
  ednevnik subjects --student ID
  ednevnik grades --student ID
  ednevnik absences --student ID
  ednevnik timeline --student ID [--page N | --all]
  ednevnik page --path '/task-schedules?student=ID'
  ednevnik sync --current [--consumer NAME]
  ednevnik sync --student ID --student ID [--consumer NAME]
  ednevnik sync --profile NAME --student ID [--student ID]
  ednevnik check --profile NAME --student ID [--student ID]
ednevnik changes [--consumer NAME]
  ednevnik consumer-register --profile NAME --consumer NAME --start earliest|latest
  ednevnik consumer-read --profile NAME --consumer NAME [--limit N]
  ednevnik consumer-ack --profile NAME --consumer NAME --token TOKEN
  ednevnik status [--profile NAME | --consumer NAME]

All data commands write JSON to stdout. Snapshots, changes, and timeline pages include a schema version. Diagnostics go to stderr.`)
}

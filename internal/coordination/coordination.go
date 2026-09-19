// Package coordination serializes all state and HTTP work for one configured
// parent account. Callers acquire one Lease per command and must not acquire a
// second lease while holding it.
package coordination

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	DefaultDailyBudget = 200
	DefaultRequestPace = 2500 * time.Millisecond
	DefaultLockWait    = 30 * time.Second
)

var (
	ErrBudgetExhausted = errors.New("daily request budget exhausted")
	ErrServerCooldown  = errors.New("server cooldown is active")
	ErrLockTimeout     = errors.New("account coordination wait expired")
	ErrInvalidState    = errors.New("invalid persisted request-policy state")
)

type Config struct {
	DailyBudget int
	RequestPace time.Duration
	LockWait    time.Duration
	Now         func() time.Time
}

func DefaultConfig() Config {
	return Config{DailyBudget: DefaultDailyBudget, RequestPace: DefaultRequestPace, LockWait: DefaultLockWait, Now: time.Now}
}

func ConfigFromEnv() (Config, error) {
	c := DefaultConfig()
	var err error
	if c.DailyBudget, err = nonnegativeIntEnv("EDNEVNIK_DAILY_REQUEST_LIMIT", c.DailyBudget); err != nil {
		return Config{}, err
	}
	if c.RequestPace, err = nonnegativeDurationEnv("EDNEVNIK_REQUEST_INTERVAL", c.RequestPace); err != nil {
		return Config{}, err
	}
	if c.LockWait, err = positiveDurationEnv("EDNEVNIK_COORDINATION_WAIT", c.LockWait); err != nil {
		return Config{}, err
	}
	return c, nil
}

type Namespace struct{ Profile, Origin string }

func (n Namespace) Key() string {
	sum := sha256.Sum256([]byte(n.Profile + "\x00" + n.Origin))
	return n.Profile + "-" + hex.EncodeToString(sum[:12])
}

type Coordinator struct {
	root      string
	namespace Namespace
	config    Config
}

func (c *Coordinator) StateDir() string {
	return filepath.Join(c.root, "coordination", c.namespace.Key())
}

func New(root string, namespace Namespace, config Config) (*Coordinator, error) {
	if root == "" || namespace.Profile == "" || namespace.Origin == "" {
		return nil, errors.New("coordination requires state root, profile, and origin")
	}
	if config.DailyBudget < 0 || config.RequestPace < 0 || config.LockWait <= 0 {
		return nil, errors.New("invalid coordination configuration")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	for _, r := range namespace.Profile {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return nil, errors.New("coordination profile must contain lowercase letters, digits, and underscores")
		}
	}
	return &Coordinator{root: root, namespace: namespace, config: config}, nil
}

type Lease struct {
	file      *os.File
	statePath string
	config    Config
	released  bool
}

func (c *Coordinator) Acquire(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := c.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "command.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, c.config.LockWait)
	defer cancel()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lease{file: f, statePath: filepath.Join(dir, "request-policy.json"), config: c.config}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		t := time.NewTimer(25 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			t.Stop()
			f.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrLockTimeout
		case <-t.C:
		}
	}
}

func (l *Lease) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

type policyState struct {
	WindowStart   time.Time `json:"window_start"`
	Count         int       `json:"count"`
	LastRequest   time.Time `json:"last_request,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
}

type Refusal struct {
	Cause   error
	RetryAt time.Time
}

func (e *Refusal) Error() string {
	return fmt.Sprintf("%v; retry at %s", e.Cause, e.RetryAt.UTC().Format(time.RFC3339))
}
func (e *Refusal) Unwrap() error { return e.Cause }

// BeforeRequest waits for pacing and records one actual transport attempt.
// The caller must invoke the transport immediately after it returns nil.
func (l *Lease) BeforeRequest(ctx context.Context) error {
	if err := l.usable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := l.load()
	if err != nil {
		return err
	}
	now := l.config.Now().UTC()
	state = rollWindow(state, now)
	if state.CooldownUntil.After(now) {
		return &Refusal{Cause: ErrServerCooldown, RetryAt: state.CooldownUntil}
	}
	if l.config.DailyBudget > 0 && state.Count >= l.config.DailyBudget {
		return &Refusal{Cause: ErrBudgetExhausted, RetryAt: state.WindowStart.Add(24 * time.Hour)}
	}
	until := state.LastRequest.Add(l.config.RequestPace)
	if state.LastRequest.After(now) {
		until = now.Add(l.config.RequestPace)
	}
	if until.After(now) {
		if err := waitUntil(ctx, until, l.config.Now); err != nil {
			return err
		}
		now = l.config.Now().UTC()
	}
	state.Count++
	state.LastRequest = now
	if state.LastRequest.Before(state.WindowStart) {
		state.LastRequest = state.WindowStart
	}
	return l.save(state)
}

func (l *Lease) RecordResponse(resp *http.Response) error {
	if err := l.usable(); err != nil {
		return err
	}
	if resp == nil || (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) {
		return nil
	}
	state, err := l.load()
	if err != nil {
		return err
	}
	now := l.config.Now().UTC()
	until := parseRetryAfter(resp.Header.Get("Retry-After"), now)
	if until.IsZero() {
		return nil
	}
	if until.After(state.CooldownUntil) {
		state.CooldownUntil = until
	}
	return l.save(state)
}

func (l *Lease) Status() (count, limit int, retryAt time.Time, err error) {
	if err := l.usable(); err != nil {
		return 0, l.config.DailyBudget, time.Time{}, err
	}
	state, err := l.load()
	if err != nil {
		return 0, l.config.DailyBudget, time.Time{}, err
	}
	state = rollWindow(state, l.config.Now().UTC())
	if l.config.DailyBudget > 0 && state.Count >= l.config.DailyBudget {
		retryAt = state.WindowStart.Add(24 * time.Hour)
	}
	if state.CooldownUntil.After(retryAt) {
		retryAt = state.CooldownUntil
	}
	return state.Count, l.config.DailyBudget, retryAt, nil
}

func (l *Lease) usable() error {
	if l == nil || l.released {
		return errors.New("coordination lease is released")
	}
	return nil
}

func rollWindow(s policyState, now time.Time) policyState {
	if s.WindowStart.IsZero() {
		s.WindowStart = now.Truncate(24 * time.Hour)
		return s
	}
	// UTC fixed windows avoid DST ambiguity. A backwards clock never grants a reset.
	if !now.Before(s.WindowStart.Add(24 * time.Hour)) {
		return policyState{WindowStart: now.Truncate(24 * time.Hour)}
	}
	return s
}

func (l *Lease) load() (policyState, error) {
	b, err := os.ReadFile(l.statePath)
	if os.IsNotExist(err) {
		return policyState{}, nil
	}
	if err != nil {
		return policyState{}, err
	}
	var s policyState
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%w: decode: %v", ErrInvalidState, err)
	}
	if s.Count < 0 || (s.Count > 0 && (s.WindowStart.IsZero() || s.LastRequest.IsZero())) || (!s.LastRequest.IsZero() && !s.WindowStart.IsZero() && s.LastRequest.Before(s.WindowStart)) {
		return s, fmt.Errorf("%w: incoherent budget window, count, or timestamps", ErrInvalidState)
	}
	return s, nil
}

func (l *Lease) save(s policyState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.statePath), ".request-policy-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, l.statePath)
}

func waitUntil(ctx context.Context, until time.Time, now func() time.Time) error {
	d := until.Sub(now().UTC())
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func parseRetryAfter(raw string, now time.Time) time.Time {
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(now) {
		return when
	}
	return time.Time{}
}

func nonnegativeIntEnv(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	v, e := strconv.Atoi(raw)
	if e != nil || v < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return v, nil
}
func positiveDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	v, e := nonnegativeDurationEnv(name, fallback)
	if e != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return v, nil
}
func nonnegativeDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	v, e := time.ParseDuration(raw)
	if e != nil || v < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration", name)
	}
	return v, nil
}

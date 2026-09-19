package coordination

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBudgetResetUsesUTCWindowAndClockRollbackDoesNotReset(t *testing.T) {
	now := time.Date(2026, 9, 18, 23, 30, 0, 0, time.UTC)
	config := DefaultConfig()
	config.DailyBudget = 2
	config.RequestPace = 0
	config.Now = func() time.Time { return now }
	c, err := New(t.TempDir(), Namespace{Profile: "family", Origin: "https://portal.example"}, config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.BeforeRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-25 * time.Hour)
	if err := lease.BeforeRequest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.BeforeRequest(context.Background()); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("rollback granted budget: %v", err)
	}
	now = time.Date(2026, 9, 19, 0, 1, 0, 0, time.UTC)
	if err := lease.BeforeRequest(context.Background()); err != nil {
		t.Fatalf("UTC reset failed: %v", err)
	}
}

func TestInvalidPersistedCounterIsRefusedAndPreserved(t *testing.T) {
	root := t.TempDir()
	c, err := New(root, Namespace{Profile: "family", Origin: "https://portal.example"}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.StateDir(), "request-policy.json")
	original := []byte(`{"window_start":"2026-09-18T00:00:00Z","count":-100}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.BeforeRequest(context.Background()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("error=%v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Fatalf("policy changed: %q err=%v", got, err)
	}
}

func TestInvalidConfigurationIsRejected(t *testing.T) {
	t.Setenv("EDNEVNIK_DAILY_REQUEST_LIMIT", "-1")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("negative daily limit accepted")
	}
	t.Setenv("EDNEVNIK_DAILY_REQUEST_LIMIT", "1")
	t.Setenv("EDNEVNIK_REQUEST_INTERVAL", "-1s")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("negative request interval accepted")
	}
}

func TestDefaultRequestLimits(t *testing.T) {
	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.DailyBudget != DefaultDailyBudget || config.RequestPace != DefaultRequestPace {
		t.Fatalf("daily budget=%d request pace=%s", config.DailyBudget, config.RequestPace)
	}
}

func TestRequestLimitsCanBeDisabled(t *testing.T) {
	t.Setenv("EDNEVNIK_DAILY_REQUEST_LIMIT", "0")
	t.Setenv("EDNEVNIK_REQUEST_INTERVAL", "0s")
	config, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if config.DailyBudget != 0 || config.RequestPace != 0 {
		t.Fatalf("daily budget=%d request pace=%s", config.DailyBudget, config.RequestPace)
	}
}

func TestResponseWithoutRetryAfterDoesNotCreateCooldown(t *testing.T) {
	config := DefaultConfig()
	config.Now = func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) }
	c, err := New(t.TempDir(), Namespace{Profile: "family", Origin: "https://portal.example"}, config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := lease.RecordResponse(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}); err != nil {
		t.Fatal(err)
	}
	if err := lease.BeforeRequest(context.Background()); err != nil {
		t.Fatalf("request refused without Retry-After: %v", err)
	}
}

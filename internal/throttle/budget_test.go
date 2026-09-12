package throttle

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestBudgetPersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.json")
	now := func() time.Time { return time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC) }
	b1 := NewBudget(path, 2)
	b1.now = now
	if err := b1.Take(); err != nil {
		t.Fatal(err)
	}
	b2 := NewBudget(path, 2)
	b2.now = now
	if err := b2.Take(); err != nil {
		t.Fatal(err)
	}
	if err := b2.Take(); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("got %v, want ErrDailyLimit", err)
	}
}

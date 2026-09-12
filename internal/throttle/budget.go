package throttle

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrDailyLimit = errors.New("daily request limit reached; wait until tomorrow or raise EDNEVNIK_DAILY_REQUEST_LIMIT deliberately")

type Budget struct {
	mu    sync.Mutex
	path  string
	limit int
	now   func() time.Time
}

type budgetState struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

func NewBudget(path string, limit int) *Budget {
	return &Budget{path: path, limit: limit, now: time.Now}
}

func (b *Budget) Take() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit <= 0 {
		return ErrDailyLimit
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	today := b.now().Format("2006-01-02")
	state := budgetState{Date: today}
	if raw, err := os.ReadFile(b.path); err == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("decode request budget: %w", err)
		}
		if state.Date != today {
			state = budgetState{Date: today}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if state.Count >= b.limit {
		return ErrDailyLimit
	}
	state.Count++
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

func (b *Budget) Status() (date string, count, limit int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	today := b.now().Format("2006-01-02")
	state := budgetState{Date: today}
	if raw, readErr := os.ReadFile(b.path); readErr == nil {
		if err = json.Unmarshal(raw, &state); err != nil {
			return "", 0, b.limit, err
		}
		if state.Date != today {
			state = budgetState{Date: today}
		}
	} else if !os.IsNotExist(readErr) {
		return "", 0, b.limit, readErr
	}
	return state.Date, state.Count, b.limit, nil
}

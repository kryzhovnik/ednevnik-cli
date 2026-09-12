package throttle

import (
	"context"
	"sync"
	"time"
)

// Limiter serializes requests and enforces a minimum interval between them.
type Limiter struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
}

func New(interval time.Duration) *Limiter {
	return &Limiter{interval: interval, now: time.Now, sleep: sleepContext}
}

func (l *Limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if wait := l.interval - now.Sub(l.last); !l.last.IsZero() && wait > 0 {
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
	l.last = l.now()
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

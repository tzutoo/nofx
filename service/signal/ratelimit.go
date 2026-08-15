package signal

import (
	"context"
	"sync"
	"time"
)

// tokenBucket is a simple fixed-capacity, per-second-refill token bucket used
// to bound external Pineify calls to Config.PineifyRatePerMinute. It is only
// ever called from the single ingest worker, so locking is cheap and there is
// no cross-goroutine contention on the request path.
type tokenBucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	refill   float64 // tokens per second
	last     time.Time
}

// newTokenBucket creates a bucket that refills to `rate` tokens per minute.
func newTokenBucket(rate int) *tokenBucket {
	if rate <= 0 {
		rate = 1
	}
	return &tokenBucket{
		capacity: float64(rate),
		tokens:   float64(rate),
		refill:   float64(rate) / 60.0,
		last:     time.Now(),
	}
}

// acquire blocks until a token is available or ctx is cancelled.
func (b *tokenBucket) acquire(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * b.refill
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - b.tokens) / b.refill * float64(time.Second))
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

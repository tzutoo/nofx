package signal

import (
	"context"
	"sync"
	"time"
)

// tokenBucket bounds external Pineify calls to Config.PineifyRatePerMinute and
// PACES them so the instantaneous rate never exceeds that per-minute ceiling.
// It is only ever called from the single ingest worker, so locking is cheap and
// there is no cross-goroutine contention on the request path.
type tokenBucket struct {
	mu         sync.Mutex
	capacity   float64
	tokens     float64
	refill     float64       // tokens per second
	minSpacing time.Duration // minimum time between consecutive grants
	last       time.Time     // last refill-accounting time
	lastGrant  time.Time     // last time a token was granted
}

// newTokenBucket creates a pacing bucket that admits at most `rate` calls per
// minute. It starts EMPTY (not pre-filled): pre-filling used to let the first
// `rate` calls fire back-to-back in a burst that tripped Pineify's wall-clock
// limit even though the per-ingest budget was respected. minSpacing (60/rate)
// additionally guarantees every grant is spaced 60/rate apart, so even a bucket
// that has idled back to full capacity never bursts past rate/min.
func newTokenBucket(rate int) *tokenBucket {
	if rate <= 0 {
		rate = 1
	}
	return &tokenBucket{
		capacity:   float64(rate),
		tokens:     0, // do not pre-fill; no startup burst
		refill:     float64(rate) / 60.0,
		minSpacing: time.Duration(60.0 / float64(rate) * float64(time.Second)),
		last:       time.Now(),
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

		// Wait until a token has refilled...
		wait := time.Duration(0)
		if b.tokens < 1 {
			wait = time.Duration((1 - b.tokens) / b.refill * float64(time.Second))
		}
		// ...and until the minimum inter-call spacing since the last grant, so a
		// full-capacity (idled) bucket still never bursts.
		if sinceGrant := now.Sub(b.lastGrant); sinceGrant < b.minSpacing {
			if d := b.minSpacing - sinceGrant; d > wait {
				wait = d
			}
		}
		if wait > 0 {
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		b.tokens--
		b.lastGrant = now
		b.mu.Unlock()
		return nil
	}
}

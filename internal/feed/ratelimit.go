package feed

import (
	"math"
	"sync"
	"time"
)

// Limits are per-IP actions per hour.
type Limits struct {
	Uploads int
	Posts   int
	Reports int
}

var DefaultLimits = Limits{Uploads: 20, Posts: 10, Reports: 30}

type bucketKey struct {
	class string
	ip    string
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// limiter is an in-process token bucket per (action, IP hash). Each Cloud Run
// instance keeps its own, so the effective limit scales with instance count.
type limiter struct {
	mu      sync.Mutex
	buckets map[bucketKey]*tokenBucket
	perHour map[string]int
	now     func() time.Time
}

func newLimiter(l Limits, now func() time.Time) *limiter {
	return &limiter{
		buckets: map[bucketKey]*tokenBucket{},
		perHour: map[string]int{"uploads": l.Uploads, "posts": l.Posts, "reports": l.Reports},
		now:     now,
	}
}

// allow takes one token, or reports how long until one is available.
func (l *limiter) allow(class, ip string) (bool, time.Duration) {
	capacity := float64(l.perHour[class])
	if capacity <= 0 {
		return true, 0
	}
	rate := capacity / float64(time.Hour)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 50000 {
		l.prune(now)
	}
	k := bucketKey{class, ip}
	b, ok := l.buckets[k]
	if !ok {
		b = &tokenBucket{tokens: capacity, last: now}
		l.buckets[k] = b
	}
	b.tokens = math.Min(capacity, b.tokens+float64(now.Sub(b.last))*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate)
}

func (l *limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > time.Hour {
			delete(l.buckets, k)
		}
	}
}

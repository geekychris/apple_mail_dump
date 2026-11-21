package server

import (
	"sync"
	"time"
)

// Limiter is a per-source-address token bucket plus a failed-auth counter
// that locks an address out for a minute after too many bad tokens.
type Limiter struct {
	Rate         float64 // tokens per second
	Burst        float64
	MaxAuthFails int           // per window
	AuthWindow   time.Duration // window and lockout length

	mu      sync.Mutex
	buckets map[string]*bucket
	fails   map[string]*failWindow
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

type failWindow struct {
	start time.Time
	n     int
}

// NewLimiter returns a limiter with the given rate and burst.
func NewLimiter(rate, burst float64) *Limiter {
	return &Limiter{Rate: rate, Burst: burst, MaxAuthFails: 10, AuthWindow: time.Minute,
		buckets: map[string]*bucket{}, fails: map[string]*failWindow{}, now: time.Now}
}

// Allow takes one token for key.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 4096 {
			l.buckets = map[string]*bucket{} // crude bound on memory
		}
		b = &bucket{tokens: l.Burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.Rate
	if b.tokens > l.Burst {
		b.tokens = l.Burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// AuthFailed records a bad token from key.
func (l *Limiter) AuthFailed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	f, ok := l.fails[key]
	if !ok || now.Sub(f.start) > l.AuthWindow {
		if len(l.fails) > 4096 {
			l.fails = map[string]*failWindow{}
		}
		f = &failWindow{start: now}
		l.fails[key] = f
	}
	f.n++
}

// AuthBlocked reports whether key is locked out.
func (l *Limiter) AuthBlocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.fails[key]
	if !ok {
		return false
	}
	if l.now().Sub(f.start) > l.AuthWindow {
		delete(l.fails, key)
		return false
	}
	return f.n >= l.MaxAuthFails
}

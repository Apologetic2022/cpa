package relay

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// AccountLimiter enforces the per-account S2 rate ceiling (docs 6.8) — a first-class
// design limit, not an option:
//  1. concurrency = 1 turn in flight per account (strict per-session serialization);
//  2. fixed per-account token budget, seeded per account at 25-35% of the plan cap,
//     never adapted from realtime ratelimit headers (C7 — Anthropic controls that input);
//  3. metering at upstream-request granularity from streaming usage deltas (F12);
//  4. diurnal silence imported from the account timezone (never self-chosen);
//  5. overflow waits in a bounded per-account FIFO, then 429 + Retry-After — it never
//     spills to another account.
type AccountLimiter struct {
	accountID string

	sem      chan struct{} // capacity 1: the single in-flight turn
	queueSem chan struct{} // capacity queueDepth: bounded FIFO slots

	budget5h     int64
	budget7d     int64 // 0 = window disabled
	opusBudget5h int64 // 0 = Opus shares the Sonnet bucket

	sonnet5h *rollingCounter
	sonnet7d *rollingCounter
	opus5h   *rollingCounter

	quietEnabled bool
	loc          *time.Location
	quietStartMin int // per-account jittered quiet window start (minutes from midnight)
	quietEndMin   int // per-account jittered quiet window end

	maxWait time.Duration
	rng     *rand.Rand
	mu      sync.Mutex // guards rng
}

// LimiterSet owns one AccountLimiter per account, built from config.
type LimiterSet struct {
	cfg       config.RelayConfig
	queueDepth int
	mu        sync.Mutex
	accounts  map[string]*AccountLimiter
}

// NewLimiterSet builds the set from relay config.
func NewLimiterSet(cfg config.RelayConfig) *LimiterSet {
	qd := cfg.QueueDepth
	if qd <= 0 {
		qd = 8
	}
	return &LimiterSet{cfg: cfg, queueDepth: qd, accounts: make(map[string]*AccountLimiter)}
}

// For returns (creating on first use) the limiter bound to an account ID.
func (s *LimiterSet) For(accountID string) *AccountLimiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.accounts[accountID]; ok {
		return l
	}
	l := newAccountLimiter(accountID, s.cfg, s.queueDepth)
	s.accounts[accountID] = l
	return l
}

func newAccountLimiter(accountID string, cfg config.RelayConfig, queueDepth int) *AccountLimiter {
	// Per-account seeded RNG (docs 7: all pacing derived from independent per-account
	// seeds; no fleet-uniform 25% flat or global cron).
	seedSum := sha256.Sum256([]byte("relay-limiter:" + accountID))
	seed := int64(binary.BigEndian.Uint64(seedSum[:8]))
	rng := rand.New(rand.NewSource(seed))

	cap5h := cfg.Limits.PlanCap5h
	if cap5h <= 0 {
		cap5h = 220000
	}
	// Fixed budget at a seeded 25-35% of plan ceiling (S9); never re-derive from headers.
	factor := 0.25 + rng.Float64()*0.10

	var opusBudget int64
	if cfg.Limits.OpusPlanCap5h > 0 {
		opusBudget = int64(float64(cfg.Limits.OpusPlanCap5h) * factor)
	}

	tz := strings.TrimSpace(cfg.Limits.DefaultTimezone)
	if tz == "" {
		tz = "UTC"
	}
	if cfg.Limits.AccountTimezone != nil {
		if per, ok := cfg.Limits.AccountTimezone[accountID]; ok && strings.TrimSpace(per) != "" {
			tz = strings.TrimSpace(per)
		}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}

	// Quiet window 23:00 -> 08:00 local with per-account seeded boundary jitter (+/-45min):
	// fleet accounts must not share synchronized silence edges.
	jitter := func() int { return int(rng.Intn(91)) - 45 }

	return &AccountLimiter{
		accountID:     accountID,
		sem:           make(chan struct{}, 1),
		queueSem:      make(chan struct{}, queueDepth),
		budget5h:      int64(float64(cap5h) * factor),
		budget7d:      int64(float64(cfg.Limits.PlanCap7d) * factor),
		opusBudget5h:  opusBudget,
		sonnet5h:      newRollingCounter(5 * time.Hour),
		sonnet7d:      newRollingCounter(7 * 24 * time.Hour),
		opus5h:        newRollingCounter(5 * time.Hour),
		quietEnabled:  cfg.Limits.QuietHoursEnabled(),
		loc:           loc,
		quietStartMin: 23*60 + jitter(),
		quietEndMin:   8*60 + jitter(),
		maxWait:       30 * time.Second,
		rng:           rng,
	}
}

// Acquire blocks until the account's single in-flight slot and budget are available,
// the bounded FIFO is exhausted (429 + Retry-After), or ctx is done. model selects the
// bucket the turn will draw from (Opus traffic is counted separately, docs 6.8.5).
// On success it returns a release func that must be called exactly once at turn end.
func (l *AccountLimiter) Acquire(ctx context.Context, estTokens int64, model string) (func(), error) {
	// Diurnal silence (docs 6.8.6): queue-until-active, bounded by maxWait.
	if l.quietEnabled {
		if wait := l.quietWait(time.Now().In(l.loc)); wait > 0 {
			if wait > l.maxWait {
				return nil, &QuotaError{Reason: "account is within its quiet-hours window (timezone-derived silence)", RetryAfter: int(wait.Seconds())}
			}
			if !sleepCtx(ctx, wait) {
				return nil, ctx.Err()
			}
		}
	}

	// Bounded FIFO slot first so waiters are capped; without a slot -> 429.
	select {
	case l.queueSem <- struct{}{}:
	default:
		return nil, &QuotaError{Reason: "account turn queue is full; bounded FIFO exhausted (no spillover to other accounts)", RetryAfter: 5}
	}
	releaseQueue := func() { <-l.queueSem }

	waitCtx, cancel := context.WithTimeout(ctx, l.maxWait)
	defer cancel()

	// Single in-flight turn (docs 6.8.1: strict serialization per account).
	select {
	case l.sem <- struct{}{}:
	case <-waitCtx.Done():
		releaseQueue()
		return nil, &QuotaError{Reason: "account already has a turn in flight; concurrency is 1 per account", RetryAfter: 3}
	}

	// Fixed budget check (C7): spending beyond the seeded budget queues, never spills.
	if retryAfter, over := l.budgetExceeded(estTokens, model); over {
		<-l.sem
		releaseQueue()
		return nil, &QuotaError{Reason: "account fixed token budget exhausted (seeded 25-35% of plan ceiling)", RetryAfter: retryAfter}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.sem
			releaseQueue()
		})
	}, nil
}

// budgetExceeded reports whether spending estTokens on model would cross the fixed
// budget for that model's bucket (Opus is separate when configured), and the seconds
// until the oldest spend exits the relevant window (Retry-After hint).
func (l *AccountLimiter) budgetExceeded(estTokens int64, model string) (int, bool) {
	now := time.Now()
	if isOpusModel(model) && l.opusBudget5h > 0 {
		if l.opus5h.Sum(now)+estTokens > l.opusBudget5h {
			return l.opus5h.RetryAfter(now), true
		}
	} else if l.sonnet5h.Sum(now)+estTokens > l.budget5h {
		return l.sonnet5h.RetryAfter(now), true
	}
	if l.budget7d > 0 && l.sonnet7d.Sum(now)+estTokens > l.budget7d {
		return l.sonnet7d.RetryAfter(now), true
	}
	return 0, false
}

// isOpusModel classifies the model bucket (docs 6.8.5: Sonnet/Opus counted separately).
func isOpusModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "opus")
}

// OnUsage records incremental usage from streaming usage deltas (F12): tokens are
// accounted as they arrive, including on aborted turns. Opus traffic goes to the
// separate Opus bucket (docs 6.8.5: Sonnet/Opus counted separately).
func (l *AccountLimiter) OnUsage(model string, input, output, cacheRead int64) {
	total := input + output + cacheRead
	if total <= 0 {
		return
	}
	now := time.Now()
	if isOpusModel(model) {
		l.opus5h.Add(total, now)
		if l.opusBudget5h <= 0 {
			l.sonnet5h.Add(total, now)
		}
	} else {
		l.sonnet5h.Add(total, now)
	}
	l.sonnet7d.Add(total, now)
}

// quietWait returns how long until the account's quiet window ends, or 0 if now is
// inside active hours. Window edges carry per-account seeded jitter.
func (l *AccountLimiter) quietWait(localNow time.Time) time.Duration {
	mins := localNow.Hour()*60 + localNow.Minute()
	start, end := l.quietStartMin, l.quietEndMin
	// Normalize into [0, 1440).
	norm := func(m int) int { return ((m % 1440) + 1440) % 1440 }
	start, end = norm(start), norm(end)
	inQuiet := func(m int) bool {
		if start <= end { // does not wrap midnight
			return m >= start && m < end
		}
		return m >= start || m < end
	}
	if !inQuiet(mins) {
		return 0
	}
	// Minutes until window end.
	var delta int
	if mins < end {
		delta = end - mins
	} else {
		delta = 1440 - mins + end
	}
	return time.Duration(delta) * time.Minute
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// rollingCounter is a sliding-window token counter.
type rollingCounter struct {
	mu     sync.Mutex
	window time.Duration
	events []usageEvent
}

type usageEvent struct {
	at time.Time
	n  int64
}

func newRollingCounter(window time.Duration) *rollingCounter {
	return &rollingCounter{window: window}
}

func (r *rollingCounter) Add(n int64, at time.Time) {
	r.mu.Lock()
	r.events = append(r.events, usageEvent{at: at, n: n})
	r.pruneLocked(at)
	r.mu.Unlock()
}

func (r *rollingCounter) Sum(now time.Time) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	var sum int64
	for _, e := range r.events {
		sum += e.n
	}
	return sum
}

// RetryAfter estimates seconds until the window has slid enough to admit new spend.
func (r *rollingCounter) RetryAfter(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return 60
	}
	oldest := r.events[0].at
	d := oldest.Add(r.window).Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return int(d.Seconds())
}

func (r *rollingCounter) pruneLocked(now time.Time) {
	cut := now.Add(-r.window)
	i := 0
	for i < len(r.events) && r.events[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		r.events = append(r.events[:0], r.events[i:]...)
	}
}

// sessionKeyFor derives the stable conversation key (docs 2.2/6.3):
// xxhash64(tenant_id ‖ normalized first user text), unless the tenant supplied an
// explicit X-Relay-Session override.
func sessionKeyFor(tenantID, override string, payload []byte) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	text := firstUserText(payload)
	norm := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	if len(norm) > 512 {
		norm = norm[:512]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(tenantID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(norm))
	return "relay-" + strings.ToLower(hexEncode64(h.Sum64()))
}

func hexEncode64(v uint64) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&0xF]
		v >>= 4
	}
	return string(b[:])
}

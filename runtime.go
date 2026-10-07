package main

import (
	"math"
	"math/rand"
	"sync"
	"time"
)

// cooldownState is the per-model cooldown record (§7.3).
type cooldownState struct {
	Until      time.Time
	Level      int
	LastStatus int
	LastError  string
	LastAt     time.Time
}

// modelStat is the per-model usage statistic.
type modelStat struct {
	Total     int64
	OK        int64
	Fail      int64
	Status429 int64
	Status5xx int64
	LastUsed  time.Time
	Latency   time.Duration
	LatencyN  int64
}

// groupStat is the per-group aggregated request counter.
type groupStat struct {
	Total int64
	OK    int64
	Fail  int64
}

// runtimeState holds all mutable per-request runtime state. It is protected by
// a mutex and safe for concurrent use (§12).
type runtimeState struct {
	mu         sync.Mutex
	cooldowns  map[string]*cooldownState
	stats      map[string]*modelStat
	groupStats map[string]*groupStat
	pointers   map[string]int

	nowFunc func() time.Time
	rng     *rand.Rand
}

func newRuntimeState() *runtimeState {
	return &runtimeState{
		cooldowns:  make(map[string]*cooldownState),
		stats:      make(map[string]*modelStat),
		groupStats: make(map[string]*groupStat),
		pointers:   make(map[string]int),
		nowFunc:    time.Now,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// now returns the current time via the injected clock (test seam).
func (r *runtimeState) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nowFunc == nil {
		return time.Now()
	}
	return r.nowFunc()
}

// setClock injects a fake clock and deterministic RNG for tests.
func (r *runtimeState) setClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nowFunc = now
	r.rng = rand.New(rand.NewSource(1))
}

// isCooling reports whether a model is currently cooling (not disabled).
func (r *runtimeState) isCooling(model string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cds, ok := r.cooldowns[model]; ok {
		return cds.Until.After(r.clock())
	}
	return false
}

// coolingInfo returns the cooldown record for a model (may be zero).
func (r *runtimeState) coolingInfo(model string) cooldownState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cds, ok := r.cooldowns[model]; ok {
		return *cds
	}
	return cooldownState{}
}

// clock returns the logical now without locking (caller must hold mu).
func (r *runtimeState) clock() time.Time {
	if r.nowFunc == nil {
		return time.Now()
	}
	return r.nowFunc()
}

// recordCooldown applies exponential backoff for a failed attempt.
// status is the HTTP status (0 when there was a transport error / timeout).
// It returns the computed cooldown duration (rounded to seconds).
func (r *runtimeState) recordCooldown(model string, status int, errMsg string, cfg config) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock()
	soft := containsInt(cfg.Cooldown.Statuses, status)
	transient := containsInt(cfg.Cooldown.TransientStatuses, status) || (status == 0)

	if !soft && !transient {
		// Other 4xx: only stats, no cooldown.
		return 0
	}

	cc := cfg.Cooldown
	base, factor, max, jitter := cfg.CooldownBase, cc.Factor, cfg.CooldownMax, cc.Jitter
	if transient {
		base, factor, max, jitter = cfg.CooldownTransientBase, cc.TransientFactor, cfg.CooldownTransientMax, cc.Jitter
	}

	cds, ok := r.cooldowns[model]
	if !ok {
		cds = &cooldownState{}
		r.cooldowns[model] = cds
	}
	cds.Level++
	cds.LastStatus = status
	cds.LastError = errMsg
	cds.LastAt = now

	delay := computeBackoff(base, max, factor, jitter, cds.Level, r.rng)
	cds.Until = now.Add(delay)
	return delay
}

// computeBackoff implements delay = min(base * factor^(level-1), max) with
// optional jitter, rounded to seconds.
func computeBackoff(base, max time.Duration, factor, jitter float64, level int, rng *rand.Rand) time.Duration {
	raw := float64(base) * math.Pow(factor, float64(level-1))
	if d := time.Duration(raw); d > max {
		raw = float64(max)
	}
	delay := time.Duration(raw)
	if jitter > 0 && rng != nil {
		adj := 1 + (rng.Float64()*2*jitter - jitter)
		if adj < 0 {
			adj = 0
		}
		delay = time.Duration(float64(delay) * adj)
	}
	return delay.Round(time.Second)
}

// recordSuccess clears cooldown and records success stats.
func (r *runtimeState) recordSuccess(model string, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cds, ok := r.cooldowns[model]; ok {
		cds.Level = 0
		cds.Until = time.Time{}
		cds.LastError = ""
	}
	st := r.stat(model)
	st.OK++
	st.LastUsed = r.clock()
	st.Latency += latency
	st.LatencyN++
}

// recordAttempt records a single attempt (increments total + lastUsed).
func (r *runtimeState) recordAttempt(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stat(model)
	st.Total++
	st.LastUsed = r.clock()
}

// recordFailure records a failed attempt (increments fail + status buckets).
func (r *runtimeState) recordFailure(model string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.stat(model)
	st.Fail++
	if status == 429 {
		st.Status429++
	}
	if status >= 500 && status <= 599 {
		st.Status5xx++
	}
}

func (r *runtimeState) stat(model string) *modelStat {
	st, ok := r.stats[model]
	if !ok {
		st = &modelStat{}
		r.stats[model] = st
	}
	return st
}

// recordGroupAttempt / recordGroupSuccess / recordGroupFail track group totals.
func (r *runtimeState) recordGroupAttempt(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.groupStat(name)
	g.Total++
}
func (r *runtimeState) recordGroupSuccess(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groupStat(name).OK++
}
func (r *runtimeState) recordGroupFail(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groupStat(name).Fail++
}
func (r *runtimeState) groupStat(name string) *groupStat {
	g, ok := r.groupStats[name]
	if !ok {
		g = &groupStat{}
		r.groupStats[name] = g
	}
	return g
}

// pointerAdvance returns the current round-robin pointer for a group and
// advances it by 1 (§7.2).
func (r *runtimeState) pointerAdvance(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pointers[name]
	r.pointers[name] = p + 1
	return p
}

// earliestCooling returns the model with the soonest cooldown expiry among the
// given models, or "" when none are cooling.
func (r *runtimeState) earliestCooling(models []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	best := ""
	var bestUntil time.Time
	for _, m := range models {
		cds, ok := r.cooldowns[m]
		if !ok || !cds.Until.After(r.clock()) {
			continue
		}
		if best == "" || cds.Until.Before(bestUntil) {
			best = m
			bestUntil = cds.Until
		}
	}
	return best
}

// reset clears cooldown/stats/group stats per the "what" selector.
// name empty => all; what in {cooldown, stats, all}.
func (r *runtimeState) reset(name, what string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cleared := 0
	switch what {
	case "cooldown", "all":
		if name == "" {
			cleared += len(r.cooldowns)
			r.cooldowns = make(map[string]*cooldownState)
		} else if _, ok := r.cooldowns[name]; ok {
			delete(r.cooldowns, name)
			cleared++
		}
	}
	if what == "stats" || what == "all" {
		if name == "" {
			cleared += len(r.stats) + len(r.groupStats)
			r.stats = make(map[string]*modelStat)
			r.groupStats = make(map[string]*groupStat)
		} else {
			if _, ok := r.stats[name]; ok {
				delete(r.stats, name)
				cleared++
			}
			if _, ok := r.groupStats[name]; ok {
				delete(r.groupStats, name)
				cleared++
			}
		}
	}
	return cleared
}

// snapshotModels returns a copy of per-model state for the management API.
func (r *runtimeState) snapshotModels() map[string]modelStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]modelStat, len(r.stats))
	for k, v := range r.stats {
		out[k] = *v
	}
	return out
}

// snapshotCooldowns returns a copy of per-model cooldown state.
func (r *runtimeState) snapshotCooldowns() map[string]cooldownState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]cooldownState, len(r.cooldowns))
	for k, v := range r.cooldowns {
		out[k] = *v
	}
	return out
}

// snapshotGroupStats returns a copy of per-group counters.
func (r *runtimeState) snapshotGroupStats() map[string]groupStat {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]groupStat, len(r.groupStats))
	for k, v := range r.groupStats {
		out[k] = *v
	}
	return out
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// tokenUsage is a normalized token count reported by one upstream response.
//
// Only upstream-reported numbers are used: the host suppresses usage reporting
// for nested executions (a group request re-enters the host via host.model.*,
// which marks the caller ctx and skips every publish point), so a group request
// leaves no usage record anywhere else. Counting here is the only way a group's
// token consumption becomes visible. Nothing is ever estimated.
type tokenUsage struct {
	Input  int64
	Output int64
	Total  int64
}

// usagePayload mirrors the token counters of the three response shapes the
// plugin can see, depending on the protocol the client used:
//
//	openai : {"usage":{"prompt_tokens":..,"completion_tokens":..,"total_tokens":..}}
//	claude : {"usage":{"input_tokens":..,"output_tokens":..}}
//	gemini : {"usageMetadata":{"promptTokenCount":..,"candidatesTokenCount":..}}
//
// All counters are pointers so "absent" is distinguishable from "zero".
type usagePayload struct {
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`

	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`

	PromptTokenCount     *int64 `json:"promptTokenCount"`
	CandidatesTokenCount *int64 `json:"candidatesTokenCount"`
	TotalTokenCount      *int64 `json:"totalTokenCount"`
}

// usageEnvelope locates a usage object inside a response payload. openai and
// claude non-streaming bodies carry it at the top level, claude streaming carries
// it in message_start as message.usage, and gemini uses usageMetadata.
type usageEnvelope struct {
	usagePayload
	Usage         *usagePayload `json:"usage"`
	UsageMetadata *usagePayload `json:"usageMetadata"`
	Message       *struct {
		Usage *usagePayload `json:"usage"`
	} `json:"message"`
}

// normalize converts a usage object into a tokenUsage. It reports false when the
// object carries no recognizable counter at all (e.g. `"usage":null` frames).
func (u usagePayload) normalize() (tokenUsage, bool) {
	in, hasIn := firstInt(u.PromptTokens, u.InputTokens, u.PromptTokenCount)
	out, hasOut := firstInt(u.CompletionTokens, u.OutputTokens, u.CandidatesTokenCount)
	total, hasTotal := firstInt(u.TotalTokens, u.TotalTokenCount)
	if !hasIn && !hasOut && !hasTotal {
		return tokenUsage{}, false
	}
	if !hasTotal {
		total = in + out
	}
	return tokenUsage{Input: in, Output: out, Total: total}, true
}

// normalize picks the first usage object present in the envelope.
func (e usageEnvelope) normalize() (tokenUsage, bool) {
	for _, candidate := range []usagePayload{
		e.usagePayload,
		derefUsage(e.Usage),
		derefUsage(e.UsageMetadata),
		messageUsage(e.Message),
	} {
		if u, ok := candidate.normalize(); ok {
			return u, true
		}
	}
	return tokenUsage{}, false
}

func derefUsage(p *usagePayload) usagePayload {
	if p == nil {
		return usagePayload{}
	}
	return *p
}

func messageUsage(m *struct {
	Usage *usagePayload `json:"usage"`
}) usagePayload {
	if m == nil {
		return usagePayload{}
	}
	return derefUsage(m.Usage)
}

// firstInt returns the first non-nil counter.
func firstInt(vals ...*int64) (int64, bool) {
	for _, v := range vals {
		if v != nil {
			return *v, true
		}
	}
	return 0, false
}

// extractUsage pulls token usage out of a response payload. The payload is either
// a whole JSON body (non-streaming) or a stream chunk; chunks are raw JSON
// objects (the host's model stream is unframed — the host adds the `data: `
// prefix when it relays plugin payloads to the client) but a `data: ` prefix is
// tolerated anyway. When a payload carries several frames the last one with usage
// wins. Unparsable payloads are not an error: usage is best-effort telemetry.
func extractUsage(payload []byte) (tokenUsage, bool) {
	var (
		out   tokenUsage
		found bool
	)
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var env usageEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if u, ok := env.normalize(); ok {
			out, found = u, true
		}
	}
	return out, found
}

// mergeTokenUsage merges a later usage report into an earlier one, field by
// field: a counter the later report omits (or reports as zero) keeps the earlier
// value. Streaming providers split one request's counters across frames (claude
// puts input tokens in message_start and output tokens in message_delta), so
// summing frames would double-count while keeping only the last frame would lose
// the input count.
func mergeTokenUsage(acc, next tokenUsage) tokenUsage {
	if next.Input > 0 {
		acc.Input = next.Input
	}
	if next.Output > 0 {
		acc.Output = next.Output
	}
	if next.Total > 0 {
		acc.Total = next.Total
	}
	if acc.Total < acc.Input+acc.Output {
		acc.Total = acc.Input + acc.Output
	}
	return acc
}

// tokenStat accumulates upstream-reported token usage.
//
// Reqs counts responses that actually reported usage, so "0 tokens" can be told
// apart from "no traffic" and from "traffic whose upstream reports no usage".
type tokenStat struct {
	Input    int64     `json:"input"`
	Output   int64     `json:"output"`
	Total    int64     `json:"total"`
	Reqs     int64     `json:"reqs"`
	LastUsed time.Time `json:"last_used_at"`
}

// add accumulates one response's usage.
func (t *tokenStat) add(u tokenUsage, now time.Time) {
	t.Input += u.Input
	t.Output += u.Output
	t.Total += u.Total
	t.Reqs++
	t.LastUsed = now
}

// ---- per-request recording ----

// recordTokens extracts upstream-reported usage from a member response payload
// and adds it to the counters of the called group and of the member. The group
// credited is the one the client called: nested groups are not credited
// separately, so summing all groups never double-counts one request.
func (p *plugin) recordTokens(groupName, member string, payload []byte) {
	u, ok := extractUsage(payload)
	if !ok {
		return
	}
	p.rt.recordTokens(groupName, member, u)
}

// ---- persistence ----
//
// Counters are persisted so "这个组一共用了多少 token" survives a restart; the
// in-memory counters (cooldown/requests) deliberately do not. The file is
// machine-written telemetry and is separate from groups.yaml, which stays
// hand-editable configuration.

// statsFile is the on-disk form of the token counters.
type statsFile struct {
	Version int                  `json:"version"`
	Updated int64                `json:"updated_at"`
	Groups  map[string]tokenStat `json:"groups"`
	Models  map[string]tokenStat `json:"models"`
}

// statsStore reads and writes the token statistics file.
type statsStore struct {
	path string
}

func newStatsStore(path string) *statsStore {
	return &statsStore{path: path}
}

// load reads the stats file. A missing file returns os.ErrNotExist so callers
// can stay silent on a fresh install.
func (s *statsStore) load() (statsFile, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return statsFile{}, err
	}
	var f statsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return statsFile{}, fmt.Errorf("解析 token 统计文件失败: %w", err)
	}
	return f, nil
}

// save writes the stats file atomically (temp file + rename, 0600).
func (s *statsStore) save(f statsFile) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 token 统计失败: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 token 统计目录失败: %w", err)
	}
	abs, err := filepath.Abs(s.path)
	if err != nil {
		abs = s.path
	}
	tmp, err := os.CreateTemp(dir, ".stats-*.json.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("设置临时文件权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("替换 token 统计文件失败: %w", err)
	}
	return nil
}

// loadTokenStats restores persisted counters. A missing file is normal (fresh
// install); a corrupt one is a warning, never fatal.
func (p *plugin) loadTokenStats() {
	p.mu.RLock()
	store := p.statsStore
	p.mu.RUnlock()
	if store == nil {
		return
	}
	f, err := store.load()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.log.warn("token 统计加载失败，从零开始", map[string]any{"error": err.Error()})
		}
		return
	}
	p.rt.restoreTokens(f.Models, f.Groups)
	p.log.info("token 统计已恢复", map[string]any{"groups": len(f.Groups), "members": len(f.Models)})
}

// flushTokenStats writes the counters unconditionally and returns any error.
func (p *plugin) flushTokenStats() error {
	p.mu.RLock()
	store := p.statsStore
	p.mu.RUnlock()
	if store == nil {
		return nil
	}
	f := statsFile{
		Version: 1,
		Updated: p.rt.now().Unix(),
		Groups:  p.rt.snapshotGroupTokens(),
		Models:  p.rt.snapshotTokens(),
	}
	if err := store.save(f); err != nil {
		p.log.warn("token 统计落盘失败", map[string]any{"error": err.Error()})
		return err
	}
	return nil
}

// flushTokenStatsIfDue writes the counters when the configured interval has
// elapsed and something changed since the last write.
func (p *plugin) flushTokenStatsIfDue() {
	p.mu.RLock()
	store, interval := p.statsStore, p.conf().StatsFlushIntervalDur
	p.mu.RUnlock()
	if store == nil || interval <= 0 {
		return
	}
	writes := p.rt.tokenUpdateCount()

	p.statsMu.Lock()
	due := writes != p.statsFlushedWrites && p.rt.now().Sub(p.statsLastFlush) >= interval
	p.statsMu.Unlock()
	if !due {
		return
	}
	if err := p.flushTokenStats(); err != nil {
		return
	}
	p.statsMu.Lock()
	p.statsFlushedWrites = writes
	p.statsLastFlush = p.rt.now()
	p.statsMu.Unlock()
}

// startStatsFlusher starts the periodic writer once per process. The tick is
// fixed at one second and the configured interval is checked inside, so changing
// stats_flush_interval does not require restarting the goroutine.
func (p *plugin) startStatsFlusher() {
	p.statsFlusherOnce.Do(func() {
		stop := make(chan struct{})
		p.statsMu.Lock()
		p.statsFlusherStop = stop
		p.statsMu.Unlock()
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					p.flushTokenStatsIfDue()
				}
			}
		}()
	})
}

// stopStatsFlusher stops the periodic writer. It is used by tests; a stopped
// flusher is not restarted (startStatsFlusher only ever runs once).
func (p *plugin) stopStatsFlusher() {
	p.statsMu.Lock()
	stop := p.statsFlusherStop
	p.statsFlusherStop = nil
	p.statsMu.Unlock()
	if stop != nil {
		close(stop)
	}
}

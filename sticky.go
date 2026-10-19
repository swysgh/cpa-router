package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// stickyKey derives the session-affinity key for one request. An empty result
// means "no stickiness for this request", which keeps plain round-robin.
//
// Header sources win over the body fingerprint: a client that sends an explicit
// session id knows its conversation boundaries better than we can infer them.
func (p *plugin) stickyKey(req *rpcExecutorRequest) string {
	if !p.cfg.Sticky {
		return ""
	}
	for _, h := range p.cfg.StickyHeaders {
		if v := strings.TrimSpace(req.Headers.Get(h)); v != "" {
			return "h:" + v
		}
	}
	if !p.cfg.StickyBodyFingerprint {
		return ""
	}
	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	return bodyFingerprint(body)
}

// bodyFingerprint hashes the conversation root: the instructions (system
// prompt) plus the first user message. Later turns are ignored on purpose, so
// every request of one conversation maps to the same key, while separate
// conversations and one-shot requests map to different keys.
func bodyFingerprint(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		System       json.RawMessage   `json:"system"`
		Instructions json.RawMessage   `json:"instructions"`
		Messages     []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	instructions := firstText(probe.Instructions)
	if instructions == "" {
		instructions = firstText(probe.System)
	}
	var firstUser string
	seenUser := false
	for _, raw := range probe.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch m.Role {
		case "system", "developer":
			if instructions == "" {
				instructions = firstText(m.Content)
			}
		case "user":
			// Freeze on the FIRST user message even when it carries no text
			// (image-only opening turn): taking the first user message that
			// happens to have text would re-key the conversation as soon as a
			// later turn adds words.
			if !seenUser {
				seenUser = true
				firstUser = firstText(m.Content)
				if firstUser == "" {
					firstUser = "raw:" + digestBytes(m.Content, 4096)
				}
			}
		}
		if seenUser {
			break
		}
	}
	if instructions == "" && firstUser == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cpa-router-sticky-v1\x00" +
		truncateRunes(instructions, 50) + "\x00" + truncateRunes(firstUser, 200)))
	return "b:" + hex.EncodeToString(sum[:])[:16]
}

// firstText extracts plain text from the shapes content can take: a string, a
// list of typed parts (claude/openai multimodal), or an object with a text
// field (responses API).
func firstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if t := strings.TrimSpace(p.Text); t != "" {
				b.WriteString(t)
				b.WriteString("\n")
			}
		}
		return strings.TrimSpace(b.String())
	}
	var obj struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.Text)
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// digestBytes hashes at most limit bytes of raw content. It backs the
// image-only opening turn case: there is no text to hash, but the raw content
// is stable across turns, and capping it keeps a multi-megabyte base64 image
// from being hashed on every request.
func digestBytes(b []byte, limit int) string {
	if len(b) > limit {
		b = b[:limit]
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

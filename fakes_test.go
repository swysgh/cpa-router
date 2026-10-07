package main

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost is a test double implementing hostCaller. It scripts responses by
// method and records every call for assertions.
type fakeHost struct {
	mu       sync.Mutex
	calls    []hostCall
	handlers map[string]func(payload json.RawMessage) (json.RawMessage, error)
	// logCalls records host.log invocations.
	logCalls []map[string]any
}

type hostCall struct {
	Method  string
	Payload json.RawMessage
}

func newFakeHost() *fakeHost {
	return &fakeHost{handlers: map[string]func(payload json.RawMessage) (json.RawMessage, error){}}
}

// okResult wraps v into a full ok envelope (mirroring how the real host returns
// the result: {ok:true, result: <v>}).
func okResult(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return okEnvelopeJSON(raw)
}

func (h *fakeHost) Call(method string, payload any) (json.RawMessage, error) {
	raw, _ := json.Marshal(payload)
	h.mu.Lock()
	h.calls = append(h.calls, hostCall{Method: method, Payload: raw})
	h.mu.Unlock()

	if method == "host.log" {
		var l map[string]any
		_ = json.Unmarshal(raw, &l)
		h.mu.Lock()
		h.logCalls = append(h.logCalls, l)
		h.mu.Unlock()
	}

	var out json.RawMessage
	if fn, ok := h.handlers[method]; ok {
		var err error
		out, err = fn(raw)
		if err != nil {
			return nil, err
		}
	} else {
		out = json.RawMessage("null")
	}

	// Mirror the real bridge: a host error envelope becomes a Go error carrying
	// the host-reported http_status (§9). On success we return the inner
	// "result" field (not the whole envelope) so plugin code that unmarshals the
	// result directly behaves identically to the real hostCaller.
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return out, nil
	}
	if !env.OK {
		he := &hostError{}
		if env.Error != nil {
			he.Code = env.Error.Code
			he.Message = env.Error.Message
			he.HTTPStatus = env.Error.HTTPStatus
		}
		return nil, he
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func (h *fakeHost) setHandler(method string, fn func(payload json.RawMessage) (json.RawMessage, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[method] = fn
}

func (h *fakeHost) callsFor(method string) []hostCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hostCall
	for _, c := range h.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHost) resetCalls() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = nil
}

// fixedClock returns a clock function pinned to t that advances by step per call.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// --- helpers to build the host.model.execute / stream responses ---

func hostExecuteResult(status int, body []byte, headers map[string][]string) json.RawMessage {
	if body == nil {
		body = []byte("{}")
	}
	hh := map[string][]string{}
	for k, v := range headers {
		hh[k] = v
	}
	// The SDK HostModelExecutionResponse.Body is []byte, so JSON encodes it as
	// a base64 string. Marshal the whole response with proper []byte encoding.
	resp := pluginapi.HostModelExecutionResponse{
		StatusCode: status,
		Headers:    hh,
		Body:       body,
	}
	result, _ := json.Marshal(resp)
	return okEnvelopeJSON(result)
}

func hostErrorResult(code string, msg string, status int) json.RawMessage {
	env := map[string]any{
		"ok": false,
		"error": map[string]any{
			"code":        code,
			"message":     msg,
			"http_status": status,
		},
	}
	raw, _ := json.Marshal(env)
	return raw
}

func hostStreamResult(status int, streamID string) json.RawMessage {
	resp := map[string]any{
		"status_code": status,
		"headers":     map[string]any{},
		"stream_id":   streamID,
	}
	result, _ := json.Marshal(resp)
	return okEnvelopeJSON(result)
}

// hostStreamReadResult wraps a stream-read chunk result in an ok envelope.
func hostStreamReadResult(payload string, errStr string, done bool) json.RawMessage {
	resp := map[string]any{
		"payload": []byte(payload),
		"error":   errStr,
		"done":    done,
	}
	result, _ := json.Marshal(resp)
	return okEnvelopeJSON(result)
}

// okEnvelopeJSON wraps a raw result into an ok envelope (mirrors okEnvelope).
func okEnvelopeJSON(result json.RawMessage) json.RawMessage {
	env := map[string]any{"ok": true, "result": json.RawMessage(result)}
	raw, _ := json.Marshal(env)
	return raw
}

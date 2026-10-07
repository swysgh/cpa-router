package main

import (
	"encoding/json"
	"testing"
)

func TestHostErrorStatusCode(t *testing.T) {
	he := &hostError{Code: "rl", Message: "limited", HTTPStatus: 429}
	if he.StatusCode() != 429 {
		t.Fatalf("StatusCode = %d, want 429", he.StatusCode())
	}
	if he.Error() != "limited" {
		t.Fatalf("Error() = %q", he.Error())
	}
}

func TestHostCallerErrorEnvelope(t *testing.T) {
	h := newFakeHost()
	h.setHandler("host.model.execute", func(payload json.RawMessage) (json.RawMessage, error) {
		return hostErrorResult("upstream_error", "服务不可用", 429), nil
	})
	_, err := h.Call("host.model.execute", map[string]any{})
	if err == nil {
		t.Fatalf("expected error")
	}
	he := asHostError(err)
	if he == nil {
		t.Fatalf("expected *hostError, got %T", err)
	}
	if he.StatusCode() != 429 {
		t.Fatalf("hostError status = %d, want 429", he.StatusCode())
	}
	if he.Message != "服务不可用" {
		t.Fatalf("hostError message = %q", he.Message)
	}
}

func TestHostCallerOKResult(t *testing.T) {
	h := newFakeHost()
	want := map[string]any{"status_code": 200}
	h.setHandler("host.model.execute", func(payload json.RawMessage) (json.RawMessage, error) {
		return okResult(want), nil
	})
	raw, err := h.Call("host.model.execute", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["status_code"] != float64(200) {
		t.Fatalf("result = %v", got)
	}
}

func TestHostCallerLogFiltering(t *testing.T) {
	h := newFakeHost()
	// Set log level to error so debug/info/warn are filtered by the logger.
	// The logger is exercised via plugin; here just check Call records log calls.
	_, _ = h.Call("host.log", map[string]any{"level": "info", "message": "hi"})
	if len(h.logCalls) != 1 {
		t.Fatalf("expected 1 log call, got %d", len(h.logCalls))
	}
}

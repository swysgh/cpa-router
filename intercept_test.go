package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ---- helpers ----

// interceptRaw builds the payload the host sends for response.intercept_after
// (the SDK request fields flattened, Body base64-encoded because it is []byte).
func interceptRaw(t *testing.T, sourceFormat string, body []byte) []byte {
	t.Helper()
	return interceptRawWithHeaders(t, sourceFormat, nil, body)
}

// interceptRawWithHeaders is interceptRaw plus the caller's request headers.
func interceptRawWithHeaders(t *testing.T, sourceFormat string, headers http.Header, body []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(rpcResponseInterceptRequest{
		ResponseInterceptRequest: pluginapi.ResponseInterceptRequest{
			RequestID:      "req-1",
			SourceFormat:   sourceFormat,
			StatusCode:     200,
			RequestHeaders: headers,
			Body:           body,
		},
	})
	if err != nil {
		t.Fatalf("marshal intercept request: %v", err)
	}
	return raw
}

// intercept dispatches response.intercept_after and returns the decoded result.
func intercept(t *testing.T, p *plugin, sourceFormat string, body []byte) pluginapi.ResponseInterceptResponse {
	t.Helper()
	return interceptWithHeaders(t, p, sourceFormat, nil, body)
}

// interceptWithHeaders is intercept for a request carrying headers.
func interceptWithHeaders(t *testing.T, p *plugin, sourceFormat string, headers http.Header, body []byte) pluginapi.ResponseInterceptResponse {
	t.Helper()
	out, err := p.HandleMethod(pluginabi.MethodResponseInterceptAfter, interceptRawWithHeaders(t, sourceFormat, headers, body))
	if err != nil {
		t.Fatalf("response.intercept_after returned error: %v", err)
	}
	var env struct {
		OK     bool                                `json:"ok"`
		Result pluginapi.ResponseInterceptResponse `json:"result"`
		Error  *envelopeError                      `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("decode intercept envelope: %v (raw=%s)", err, out)
	}
	if !env.OK {
		t.Fatalf("intercept envelope not ok: %s", out)
	}
	return env.Result
}

// modelListJSON renders an OpenAI model list from verbatim entry literals.
func modelListJSON(t *testing.T, entries ...string) []byte {
	t.Helper()
	body := `{"object":"list","data":[` + strings.Join(entries, ",") + `]}`
	if !json.Valid([]byte(body)) {
		t.Fatalf("invalid test body: %s", body)
	}
	return []byte(body)
}

// modelIDs decodes the ids of a model-list body (also asserting it is a list).
func modelIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var root struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode model list %s: %v", body, err)
	}
	if root.Object != "list" {
		t.Fatalf("object = %q, want list", root.Object)
	}
	ids := make([]string, 0, len(root.Data))
	for _, d := range root.Data {
		ids = append(ids, d.ID)
	}
	return ids
}

// interceptPlugin builds a plugin with the given groups (and prefix) plus a
// config whose filter switch is set as requested.
func interceptPlugin(t *testing.T, prefix string, filter bool, gs ...rawGroup) *plugin {
	t.Helper()
	cfg := defaultConfig()
	cfg.NamePrefix = prefix
	cfg.FilterModels = filter
	return newTestPlugin(t, groupFile{Version: 1, Groups: gs}, cfg)
}

func enabledGroup(name string, aliases ...string) rawGroup {
	return rawGroup{Name: name, Strategy: "fallback", Enabled: true, Aliases: aliases,
		Members: []rawMember{{Model: "m1", Enabled: true}}}
}

// ---- filtering ----

// The filter keeps exactly the call names the plugin advertises: enabled group
// names and aliases with name_prefix, and nothing else — not the upstream models
// behind the groups, not disabled groups, not bare names when a prefix is set.
func TestInterceptKeepsOnlyEnabledGroupCallNames(t *testing.T) {
	p := interceptPlugin(t, "r/", true,
		enabledGroup("free-stack", "free"),
		rawGroup{Name: "off", Strategy: "fallback", Enabled: false, Aliases: []string{"off-alias"},
			Members: []rawMember{{Model: "m2", Enabled: true}}},
	)

	body := modelListJSON(t,
		`{"id":"r/free-stack","object":"model","created":1700000000,"owned_by":"cpa-router"}`,
		`{"id":"gpt-4o","object":"model"}`,
		`{"id":"r/free","object":"model"}`,
		`{"id":"free-stack","object":"model"}`,
		`{"id":"r/off","object":"model"}`,
		`{"id":"r/off-alias","object":"model"}`,
		`{"id":"m1","object":"model"}`,
	)

	got := intercept(t, p, modelListSourceFormat, body)
	if len(got.Body) == 0 {
		t.Fatal("expected a filtered body, got an empty one (host would keep the full list)")
	}
	want := []string{"r/free-stack", "r/free"}
	if ids := modelIDs(t, got.Body); !reflect.DeepEqual(ids, want) {
		t.Errorf("kept %v, want %v", ids, want)
	}
}

// Root siblings and per-model extra fields must survive the rewrite untouched.
func TestInterceptPreservesRootAndModelFields(t *testing.T) {
	p := interceptPlugin(t, "", true, enabledGroup("grp"))

	body := []byte(`{"object":"list","data":[` +
		`{"id":"grp","object":"model","created":1700000000,"owned_by":"cpa-router","x_custom":{"a":[1,2]}},` +
		`{"id":"gpt-4o","object":"model","created":1,"owned_by":"openai"}` +
		`],"extra_root":{"nested":[1,2]},"has_more":false}`)

	got := intercept(t, p, modelListSourceFormat, body)
	if len(got.Body) == 0 {
		t.Fatal("expected a filtered body")
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatalf("filtered body is not valid JSON: %v (%s)", err, got.Body)
	}
	if string(root["object"]) != `"list"` {
		t.Errorf("object = %s, want \"list\"", root["object"])
	}
	if string(root["extra_root"]) != `{"nested":[1,2]}` {
		t.Errorf("extra_root = %s, want it preserved", root["extra_root"])
	}
	if string(root["has_more"]) != "false" {
		t.Errorf("has_more = %s, want it preserved", root["has_more"])
	}

	var entries []map[string]any
	if err := json.Unmarshal(root["data"], &entries); err != nil {
		t.Fatalf("data is not an array: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("kept %d entries, want 1: %s", len(entries), root["data"])
	}
	e := entries[0]
	if e["created"] != float64(1700000000) || e["owned_by"] != "cpa-router" {
		t.Errorf("kept entry lost its fields: %v", e)
	}
	if _, ok := e["x_custom"]; !ok {
		t.Errorf("kept entry lost the unknown x_custom field: %v", e)
	}
}

// A list with none of our names still gets rewritten — to a valid empty array.
func TestInterceptNoMatchYieldsEmptyData(t *testing.T) {
	p := interceptPlugin(t, "", true, enabledGroup("grp"))

	got := intercept(t, p, modelListSourceFormat,
		modelListJSON(t, `{"id":"gpt-4o"}`, `{"id":"claude-3"}`))
	if len(got.Body) == 0 {
		t.Fatal("expected a rewritten body")
	}
	if ids := modelIDs(t, got.Body); len(ids) != 0 {
		t.Errorf("kept %v, want none", ids)
	}
	if !strings.Contains(string(got.Body), `"data":[]`) {
		t.Errorf("body = %s, want data:[]", got.Body)
	}
}

// Disabled groups and the upstream models behind a group are not callable names,
// so they are dropped too.
func TestInterceptDropsDisabledGroupAndMemberModels(t *testing.T) {
	p := interceptPlugin(t, "", true,
		rawGroup{Name: "grp", Strategy: "fallback", Enabled: false,
			Members: []rawMember{{Model: "gpt-4o-mini", Enabled: true}}},
	)

	got := intercept(t, p, modelListSourceFormat,
		modelListJSON(t, `{"id":"grp"}`, `{"id":"gpt-4o-mini"}`, `{"id":"gpt-4o"}`))
	if ids := modelIDs(t, got.Body); len(ids) != 0 {
		t.Errorf("kept %v, want none", ids)
	}
}

// With no groups at all there is nothing to advertise, so the list comes back
// empty. filter_models: false is the documented escape hatch for that.
func TestInterceptNoGroupsYieldsEmptyList(t *testing.T) {
	p := interceptPlugin(t, "", true)

	got := intercept(t, p, modelListSourceFormat, modelListJSON(t, `{"id":"gpt-4o"}`))
	if ids := modelIDs(t, got.Body); len(ids) != 0 {
		t.Errorf("kept %v, want none", ids)
	}
}

func TestInterceptFilterDisabledLeavesBodyAlone(t *testing.T) {
	p := interceptPlugin(t, "", false, enabledGroup("grp"))

	body := modelListJSON(t, `{"id":"gpt-4o"}`, `{"id":"grp"}`)
	got := intercept(t, p, modelListSourceFormat, body)
	if len(got.Body) != 0 {
		t.Fatalf("filter_models=false must not rewrite the body, got %s", got.Body)
	}
}

// ---- fail-open paths ----

// Anything that is not provably an OpenAI model list must come back as an empty
// modification (the host then keeps its own bytes). A chat/completions body must
// never be touched.
func TestInterceptPassesThroughUnrecognisedBodies(t *testing.T) {
	p := interceptPlugin(t, "", true, enabledGroup("grp"))

	cases := []struct {
		name         string
		sourceFormat string
		body         string
	}{
		{"claude source format", "claude", `{"object":"list","data":[{"id":"gpt-4o"}]}`},
		{"gemini source format", "gemini", `{"object":"list","data":[{"id":"gpt-4o"}]}`},
		{"empty source format", "", `{"object":"list","data":[{"id":"gpt-4o"}]}`},
		{"invalid json", modelListSourceFormat, `{"object":"list","data":[`},
		{"empty body", modelListSourceFormat, ``},
		{"json array root", modelListSourceFormat, `[{"id":"gpt-4o"}]`},
		{"json scalar root", modelListSourceFormat, `"list"`},
		{"chat completion", modelListSourceFormat, `{"object":"chat.completion","data":[{"id":"gpt-4o"}],"choices":[]}`},
		{"model object", modelListSourceFormat, `{"object":"model","id":"gpt-4o"}`},
		{"missing object", modelListSourceFormat, `{"data":[{"id":"gpt-4o"}]}`},
		{"missing data", modelListSourceFormat, `{"object":"list"}`},
		{"null data", modelListSourceFormat, `{"object":"list","data":null}`},
		{"object data", modelListSourceFormat, `{"object":"list","data":{"id":"gpt-4o"}}`},
		{"string data", modelListSourceFormat, `{"object":"list","data":"gpt-4o"}`},
		{"scalar entries", modelListSourceFormat, `{"object":"list","data":[1,2]}`},
		{"entry without id", modelListSourceFormat, `{"object":"list","data":[{"object":"model"}]}`},
		{"entry with numeric id", modelListSourceFormat, `{"object":"list","data":[{"id":7}]}`},
		{"stream chunk", modelListSourceFormat, `{"object":"chat.completion.chunk","choices":[]}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := intercept(t, p, c.sourceFormat, []byte(c.body))
			if len(got.Body) != 0 {
				t.Errorf("body must pass through untouched, got %s", got.Body)
			}
		})
	}
}

// A broken entry must never take a sibling down with it: entries we cannot read
// an id from are not provably ours, so they stay.
func TestInterceptKeepsUnreadableEntries(t *testing.T) {
	p := interceptPlugin(t, "", true, enabledGroup("grp"))

	got := intercept(t, p, modelListSourceFormat,
		modelListJSON(t, `{"object":"model"}`, `{"id":7}`, `"bare"`, `{"id":"grp"}`, `{"id":"other"}`))
	if len(got.Body) == 0 {
		t.Fatal("expected a filtered body")
	}
	var root struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatalf("filtered body is not valid JSON: %v (%s)", err, got.Body)
	}
	want := []string{`{"object":"model"}`, `{"id":7}`, `"bare"`, `{"id":"grp"}`}
	kept := make([]string, 0, len(root.Data))
	for _, e := range root.Data {
		kept = append(kept, string(e))
	}
	if !reflect.DeepEqual(kept, want) {
		t.Errorf("kept %v, want %v", kept, want)
	}
}

// An unparseable RPC request (or a Body that is not base64) must not turn into an
// error: the host logs a warning and skips the plugin, which would drop the
// whole response rather than just leaving it alone.
func TestInterceptMalformedRequestFailsOpen(t *testing.T) {
	p := interceptPlugin(t, "", true, enabledGroup("grp"))

	for _, raw := range []string{
		`not json at all`,
		``,
		`{"SourceFormat":"openai","Body":"!!!not-base64!!!"}`,
	} {
		out, err := p.HandleMethod(pluginabi.MethodResponseInterceptAfter, []byte(raw))
		if err != nil {
			t.Fatalf("raw %q: unexpected error %v", raw, err)
		}
		var env struct {
			OK     bool                                `json:"ok"`
			Result pluginapi.ResponseInterceptResponse `json:"result"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("raw %q: envelope %s is not JSON: %v", raw, out, err)
		}
		if !env.OK {
			t.Fatalf("raw %q: envelope not ok: %s", raw, out)
		}
		if len(env.Result.Body) != 0 {
			t.Errorf("raw %q: expected an empty body, got %s", raw, env.Result.Body)
		}
	}
}

// ---- consistency with model.register ----

// The allow-list must be exactly what model.register advertises, or a client
// would see a model in the list that the filter then deletes (or the reverse).
func TestInterceptKeepsExactlyTheRegisteredModels(t *testing.T) {
	p := interceptPlugin(t, "r/", true,
		enabledGroup("free-stack", "free"),
		rawGroup{Name: "off", Strategy: "fallback", Enabled: false,
			Members: []rawMember{{Model: "m2", Enabled: true}}},
	)

	reg := decodeModelRegistration(t, p)
	entries := make([]string, 0, len(reg.Models)+2)
	want := make([]string, 0, len(reg.Models))
	for _, m := range reg.Models {
		want = append(want, m.ID)
		entries = append(entries, fmt.Sprintf(`{"id":%q,"object":"model"}`, m.ID))
	}
	entries = append(entries, `{"id":"gpt-4o"}`, `{"id":"r/off"}`)

	got := intercept(t, p, modelListSourceFormat, modelListJSON(t, entries...))
	if ids := modelIDs(t, got.Body); !reflect.DeepEqual(ids, want) {
		t.Errorf("kept %v, want the registered %v", ids, want)
	}
}

// ---- config ----

// filter_models defaults to on and only an explicit false turns it off; a
// reconfigure must take effect on the next intercepted response.
func TestInterceptFollowsReconfigure(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "groups.yaml")
	if err := newGroupStore(statePath).save(groupFile{Version: 1, Groups: []rawGroup{enabledGroup("grp")}}); err != nil {
		t.Fatalf("seed state file: %v", err)
	}

	configure := func(p *plugin, filter string) {
		t.Helper()
		yaml := fmt.Sprintf("state_file: %q\nstats_file: \"\"\nfilter_models: %s\n", statePath, filter)
		raw, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte(yaml)})
		if err != nil {
			t.Fatalf("marshal lifecycle request: %v", err)
		}
		if err := p.configure(raw); err != nil {
			t.Fatalf("configure(%s): %v", filter, err)
		}
	}

	p := newPlugin(newFakeHost())
	configure(p, "false")
	if got := intercept(t, p, modelListSourceFormat, modelListJSON(t, `{"id":"grp"}`, `{"id":"gpt-4o"}`)); len(got.Body) != 0 {
		t.Fatalf("filter_models=false must leave the body alone, got %s", got.Body)
	}

	configure(p, "true")
	got := intercept(t, p, modelListSourceFormat, modelListJSON(t, `{"id":"grp"}`, `{"id":"gpt-4o"}`))
	if ids := modelIDs(t, got.Body); !reflect.DeepEqual(ids, []string{"grp"}) {
		t.Fatalf("after reconfigure kept %v, want [grp]", ids)
	}
}

// ---- management console bypass ----

// Requests made by a management console must see the full upstream catalog: the
// trim hides upstream model names from API clients, but a picker that cannot see
// them cannot build a group. The interceptor payload carries no request URL in
// CPA v8.0.13, so the calling page's Referer (plus an opt-in User-Agent marker)
// is the only discriminator available.
func TestInterceptSkipsManagementConsoleRequests(t *testing.T) {
	p := interceptPlugin(t, "r/", true, enabledGroup("free-stack", "free"))
	body := modelListJSON(t,
		`{"id":"r/free-stack","object":"model","created":1700000000}`,
		`{"id":"gpt-4o","object":"model"}`,
		`{"id":"claude-3-5-sonnet","object":"model"}`,
	)

	consoles := []struct {
		name    string
		headers http.Header
	}{
		{"panel iframe", http.Header{"Referer": []string{"https://cpa.example.com/v0/resource/plugins/cpa-router/panel"}}},
		{"management page", http.Header{"Referer": []string{"https://cpa.example.com/management.html"}}},
		{"management api", http.Header{"Referer": []string{"https://cpa.example.com/v0/management/plugins/cpa-router/panel"}}},
		{"panel user agent", http.Header{"User-Agent": []string{"cpa-router-panel/0.7.0"}}},
		{"cpamp user agent", http.Header{"User-Agent": []string{"cpamp-panel"}}},
	}
	for _, tc := range consoles {
		t.Run(tc.name, func(t *testing.T) {
			got := interceptWithHeaders(t, p, modelListSourceFormat, tc.headers, body)
			if len(got.Body) != 0 {
				t.Fatalf("console request must pass through untouched, got %s", got.Body)
			}
		})
	}

	// Control: an ordinary API client keeps the trimmed answer.
	for _, headers := range []http.Header{
		nil,
		{"Referer": []string{"https://example.com/chat"}},
		{"User-Agent": []string{"curl/8.5.0"}},
	} {
		got := interceptWithHeaders(t, p, modelListSourceFormat, headers, body)
		if ids := modelIDs(t, got.Body); !reflect.DeepEqual(ids, []string{"r/free-stack"}) {
			t.Fatalf("api client (headers %v) kept %v, want [r/free-stack]", headers, ids)
		}
	}
}

// isInternalRequest is the bypass predicate on its own: only the markers that
// actually identify a console may match, so a client cannot slip past by sending
// a header that merely looks similar.
func TestIsInternalRequest(t *testing.T) {
	internal := []http.Header{
		{"Referer": []string{"https://host/v0/resource/plugins/cpa-router/panel"}},
		{"Referer": []string{"https://host/management.html#/plugins"}},
		{"Referer": []string{"https://host/v0/management/plugins/cpa-router/state"}},
		{"User-Agent": []string{"cpa-router-panel"}},
		{"User-Agent": []string{"cpamp-panel/1.2.3"}},
	}
	for _, h := range internal {
		if !isInternalRequest(h) {
			t.Errorf("headers %v must be treated as internal", h)
		}
	}
	external := []http.Header{
		nil,
		{},
		{"Referer": []string{"https://example.com/chat"}},
		{"Referer": []string{"https://example.com/v0/resource/plugin/other"}},
		{"User-Agent": []string{"OpenAI/Python 1.30.0"}},
	}
	for _, h := range external {
		if isInternalRequest(h) {
			t.Errorf("headers %v must not be treated as internal", h)
		}
	}
}

// ---- concurrency ----

// The intercept path is read-only and must stay race-free while a reconfigure
// swaps the config and the group list underneath it.
func TestInterceptRaceFreeUnderReconfigure(t *testing.T) {
	p := interceptPlugin(t, "", true, enabledGroup("g1", "a1"), enabledGroup("g2"))
	raw := interceptRaw(t, modelListSourceFormat,
		modelListJSON(t, `{"id":"g1"}`, `{"id":"a1"}`, `{"id":"g2"}`, `{"id":"other"}`))

	parsedA := groupsFor(t, enabledGroup("g1", "a1"), enabledGroup("g2"))
	parsedB := groupsFor(t, enabledGroup("g1"))

	var readers, writer sync.WaitGroup
	stop := make(chan struct{})
	failures := make(chan string, 64)

	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p.mu.Lock()
			p.cfg.FilterModels = i%2 == 0
			if i%3 == 0 {
				p.groups = parsedA
			} else {
				p.groups = parsedB
			}
			p.mu.Unlock()
		}
	}()

	for w := 0; w < 8; w++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				out, err := p.HandleMethod(pluginabi.MethodResponseInterceptAfter, raw)
				if err != nil {
					failures <- err.Error()
					return
				}
				var env struct {
					OK     bool                                `json:"ok"`
					Result pluginapi.ResponseInterceptResponse `json:"result"`
				}
				if err := json.Unmarshal(out, &env); err != nil || !env.OK {
					failures <- fmt.Sprintf("bad envelope: %s", out)
					return
				}
				if len(env.Result.Body) > 0 && !json.Valid(env.Result.Body) {
					failures <- fmt.Sprintf("filtered body is not JSON: %s", env.Result.Body)
					return
				}
			}
		}()
	}

	// The readers run while the writer keeps flipping the config and groups.
	readers.Wait()
	close(stop)
	writer.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}
}

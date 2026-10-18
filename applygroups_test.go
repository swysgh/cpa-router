package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// applyGroupsInput builds a group file whose declaration order is deliberately
// not alphabetical: the old implementation published this slice and only then
// sorted the same backing array outside p.mu, and that write is what raced with
// the readers.
func applyGroupsInput() groupFile {
	return groupFile{Version: 1, Groups: []rawGroup{
		enabledGroup("c"),
		enabledGroup("a"),
		enabledGroup("b"),
	}}
}

// Regression: applyGroups used to run
//
//	sort.Slice(groups, ...)
//
// after p.mu.Unlock() while p.groups still shared that backing array, so every
// reader that copies or walks p.groups under RLock — allGroupsSnapshot (/state,
// the panel), callNameSnapshot (model.register and the v0.6.0 /v1/models
// filter), resolveGroup (route/executor, which even hands out a pointer into the
// array) — could race with the reorder. The reorder must happen before the swap
// so the published slice is never mutated again, and the sorted order must still
// be what the next read sees.
func TestApplyGroupsRaceFreeUnderConcurrentReaders(t *testing.T) {
	cfg := defaultConfig()
	cfg.NamePrefix = "r/"
	p := newTestPlugin(t, applyGroupsInput(), cfg)

	raw := interceptRaw(t, modelListSourceFormat,
		modelListJSON(t, `{"id":"r/a"}`, `{"id":"r/b"}`, `{"id":"r/c"}`, `{"id":"other"}`))

	var readers, writer sync.WaitGroup
	stop := make(chan struct{})
	failures := make(chan string, 64)
	fail := func(format string, args ...any) {
		select {
		case failures <- fmt.Sprintf(format, args...):
		default:
		}
	}

	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// A fresh file per round: parseGroups allocates a new slice, so
			// every iteration publishes a brand-new backing array.
			if err := p.applyGroups(applyGroupsInput()); err != nil {
				fail("applyGroups: %v", err)
				return
			}
		}
	}()

	// The readers hammer every lock-protected view of p.groups while the writer
	// keeps re-publishing the non-alphabetical order.
	for w := 0; w < 8; w++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 300; i++ {
				snap := p.callNameSnapshot()
				seen := 0
				snap.forEachEnabledCallName(func(id string, g group) { seen++ })
				if seen == 0 || len(snap.modelInfos()) == 0 {
					fail("callNameSnapshot lost every call name")
					return
				}
				if _, ok := p.resolveGroup("r/a"); !ok {
					fail("resolveGroup(r/a) missed a group that is always present")
					return
				}
				if got := p.allGroupsSnapshot(); len(got) != 3 {
					fail("allGroupsSnapshot len = %d, want 3", len(got))
					return
				}
				out, err := p.HandleMethod(pluginabi.MethodResponseInterceptAfter, raw)
				if err != nil {
					fail("intercept: %v", err)
					return
				}
				var env struct {
					OK     bool                                `json:"ok"`
					Result pluginapi.ResponseInterceptResponse `json:"result"`
				}
				if err := json.Unmarshal(out, &env); err != nil || !env.OK {
					fail("bad intercept envelope: %s", out)
					return
				}
				if len(env.Result.Body) > 0 && !json.Valid(env.Result.Body) {
					fail("filtered body is not JSON: %s", env.Result.Body)
					return
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	writer.Wait()
	close(failures)
	for f := range failures {
		t.Error(f)
	}

	// External behavior: /state and the panel list groups straight from this
	// snapshot, so the order the next read sees must be the sorted one.
	p.mu.RLock()
	published := make([]string, 0, len(p.groups))
	for _, g := range p.groups {
		published = append(published, g.Name)
	}
	p.mu.RUnlock()
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(published, want) {
		t.Errorf("published group order = %v, want %v", published, want)
	}
}

// The sorted order is the observable contract of both publication paths: /state
// (and therefore the panel, which renders the /state payload verbatim) lists
// groups sorted by name whether they were published by a management save or by a
// host reload of a hand-written state file.
func TestGroupOrderSortedAfterSaveAndReload(t *testing.T) {
	p := newTestPlugin(t, applyGroupsInput(), defaultConfig())
	store := newGroupStore(filepath.Join(t.TempDir(), "groups.yaml"))
	p.store = store

	stateGroupNames := func() []string {
		t.Helper()
		raw, err := mgmtRaw(t, p, "GET", "/plugins/cpa-router/state", nil)
		if err != nil {
			t.Fatalf("GET /state: %v", err)
		}
		groups, _ := decodeMgmtJSON(t, raw)["groups"].([]any)
		names := make([]string, 0, len(groups))
		for _, gi := range groups {
			g, _ := gi.(map[string]any)
			name, _ := g["name"].(string)
			names = append(names, name)
		}
		return names
	}

	want := []string{"a", "b", "c"}

	if err := p.applyGroups(applyGroupsInput()); err != nil {
		t.Fatalf("applyGroups: %v", err)
	}
	if got := stateGroupNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("/state order after a management save = %v, want %v", got, want)
	}

	// A reload reads whatever order the file happens to have; the published
	// order must not flip back to the file order.
	if err := store.save(applyGroupsInput()); err != nil {
		t.Fatalf("store.save: %v", err)
	}
	p.reloadGroups()
	if got := stateGroupNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("/state order after a host reload = %v, want %v", got, want)
	}
}

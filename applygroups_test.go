package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// enabledGroup builds a raw group with one model member so the group passes
// validation and is published.
func enabledGroup(name string, aliases ...string) rawGroup {
	return rawGroup{Name: name, Strategy: "fallback", Enabled: true, Aliases: aliases,
		Members: []rawMember{{Model: "m1", Enabled: true}}}
}

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
// the panel), resolveGroup (route/executor, which even hands out a pointer into
// the array), modelRegistrationJSON (model.register) — could race with the
// reorder. The reorder must happen before the swap so the published slice is
// never mutated again, and the sorted order must still be what the next read
// sees.
func TestApplyGroupsRaceFreeUnderConcurrentReaders(t *testing.T) {
	cfg := defaultConfig()
	cfg.NamePrefix = "r/"
	p := newTestPlugin(t, applyGroupsInput(), cfg)

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
				if got := p.allGroupsSnapshot(); len(got) != 3 {
					fail("allGroupsSnapshot len = %d, want 3", len(got))
					return
				}
				if _, ok := p.resolveGroup("r/a"); !ok {
					fail("resolveGroup(r/a) missed a group that is always present")
					return
				}
				if len(p.modelRegistrationJSON().Models) == 0 {
					fail("model.register lost every call name")
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

package main

import (
	"sync"
	"testing"
)

// TestConfigSwapIsRaceFree pins the reconfigure-vs-request-path contract: the
// configuration is swapped atomically, so readers never observe a half-written
// struct. Run under -race; the pre-atomic version trips the detector here.
func TestConfigSwapIsRaceFree(t *testing.T) {
	p := newTestPlugin(t, groups3RoundRobin(), defaultConfig())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			cfg := defaultConfig()
			cfg.MaxAttempts = 1 + i%6
			p.setConfig(cfg)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			if _, err := p.expand("g", 1, map[string]bool{}, expandOpts{}); err != nil {
				t.Errorf("expand: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

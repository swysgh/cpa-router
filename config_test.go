package main

import (
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()
	if cfg.StateFile != "plugins/cpa-router/groups.yaml" {
		t.Errorf("state_file default = %q", cfg.StateFile)
	}
	if cfg.NamePrefix != "" {
		t.Errorf("name_prefix default = %q", cfg.NamePrefix)
	}
	if cfg.MaxAttempts != 6 {
		t.Errorf("max_attempts default = %d", cfg.MaxAttempts)
	}
	if cfg.AllCoolingPolicy != "wait" {
		t.Errorf("all_cooling_policy default = %q", cfg.AllCoolingPolicy)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("log_level default = %q", cfg.LogLevel)
	}
	if cfg.Cooldown.Base != "30s" {
		t.Errorf("cooldown.base default = %q", cfg.Cooldown.Base)
	}
}

func TestDecodeConfigInvalidFields(t *testing.T) {
	cases := []struct {
		yaml string
		want string
	}{
		{"reload_interval: \"abc\"\n", "reload_interval"},
		{"attempt_timeout: \"xyz\"\n", "attempt_timeout"},
		{"total_timeout: \"-1s\"\n", "total_timeout"},
		{"max_attempts: -1\n", "max_attempts"},
		{"cooldown:\n  factor: 0.5\n", "factor"},
		{"cooldown:\n  jitter: 1.5\n", "jitter"},
		{"all_cooling_policy: bogus\n", "all_cooling_policy"},
		{"log_level: trace\n", "log_level"},
		{"cooldown:\n  max: \"10s\"\n  base: \"30s\"\n", "max"},
		{"cooldown:\n  statuses: [700]\n", "statuses"},
		{"cooldown:\n  transient_statuses: [99]\n", "transient_statuses"},
		{"name_prefix: \"bad prefix!\"\n", "name_prefix"},
	}
	for _, c := range cases {
		_, err := decodeConfig([]byte(c.yaml))
		if err == nil {
			t.Errorf("expected error for %q, got nil", c.yaml)
			continue
		}
		if !containsSubstr(err.Error(), c.want) {
			t.Errorf("yaml %q: error %q does not mention %q", c.yaml, err.Error(), c.want)
		}
	}
}

func TestDecodeConfigOK(t *testing.T) {
	cfg, err := decodeConfig([]byte("name_prefix: \"r/\"\nmax_attempts: 3\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NamePrefix != "r/" {
		t.Errorf("name_prefix = %q", cfg.NamePrefix)
	}
	if cfg.MaxAttempts != 3 {
		t.Errorf("max_attempts = %d", cfg.MaxAttempts)
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

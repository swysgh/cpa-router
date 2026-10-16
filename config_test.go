package main

import (
	"testing"
	"time"
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

// Regression: validateConfig used to take the config by value, so every parsed
// duration it computed was written to a copy and thrown away — a custom
// duration in config.yaml was silently replaced by the default. Only the string
// fields (and the slices decoded straight from YAML) took effect.
func TestDecodeConfigHonorsCustomDurations(t *testing.T) {
	cfg, err := decodeConfig([]byte("attempt_timeout: \"7s\"\ntotal_timeout: \"90s\"\nreload_interval: \"9s\"\n" +
		"cooldown:\n  base: \"11s\"\n  max: \"12s\"\n  transient_base: \"13s\"\n  transient_max: \"14s\"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range []struct {
		field string
		got   time.Duration
		want  time.Duration
	}{
		{"attempt_timeout", cfg.AttemptTimeoutDur, 7 * time.Second},
		{"total_timeout", cfg.TotalTimeoutDur, 90 * time.Second},
		{"reload_interval", cfg.ReloadIntervalDur, 9 * time.Second},
		{"cooldown.base", cfg.CooldownBase, 11 * time.Second},
		{"cooldown.max", cfg.CooldownMax, 12 * time.Second},
		{"cooldown.transient_base", cfg.CooldownTransientBase, 13 * time.Second},
		{"cooldown.transient_max", cfg.CooldownTransientMax, 14 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
}

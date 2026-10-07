package main

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// config holds the effective plugin configuration. Defaults are applied by
// defaultConfig and user-supplied YAML is layered on top. Duration fields are
// stored as strings so we can produce field-named validation errors (§4).
type config struct {
	StateFile        string `yaml:"state_file"`
	NamePrefix       string `yaml:"name_prefix"`
	ReloadInterval   string `yaml:"reload_interval"`
	MaxAttempts      int    `yaml:"max_attempts"`
	AttemptTimeout   string `yaml:"attempt_timeout"`
	TotalTimeout     string `yaml:"total_timeout"`
	AllCoolingPolicy string `yaml:"all_cooling_policy"`
	MaxWait          string `yaml:"max_wait"`
	LogLevel         string `yaml:"log_level"`

	Cooldown cooldownConfig `yaml:"cooldown"`

	// Parsed durations (filled by decodeConfig / validateConfig).
	ReloadIntervalDur     time.Duration `yaml:"-"`
	AttemptTimeoutDur     time.Duration `yaml:"-"`
	TotalTimeoutDur       time.Duration `yaml:"-"`
	MaxWaitDur            time.Duration `yaml:"-"`
	CooldownBase          time.Duration `yaml:"-"`
	CooldownMax           time.Duration `yaml:"-"`
	CooldownTransientBase time.Duration `yaml:"-"`
	CooldownTransientMax  time.Duration `yaml:"-"`
}

// cooldownConfig holds the exponential-backoff parameters (durations as strings).
type cooldownConfig struct {
	Disable           bool    `yaml:"disable"`
	Statuses          []int   `yaml:"statuses"`
	Base              string  `yaml:"base"`
	Factor            float64 `yaml:"factor"`
	Max               string  `yaml:"max"`
	Jitter            float64 `yaml:"jitter"`
	TransientStatuses []int   `yaml:"transient_statuses"`
	TransientBase     string  `yaml:"transient_base"`
	TransientFactor   float64 `yaml:"transient_factor"`
	TransientMax      string  `yaml:"transient_max"`
}

// defaultConfig returns the documented default configuration.
func defaultConfig() config {
	cfg := config{
		StateFile:        "plugins/cpa-router/groups.yaml",
		NamePrefix:       "",
		ReloadInterval:   "2s",
		MaxAttempts:      6,
		AttemptTimeout:   "120s",
		TotalTimeout:     "600s",
		AllCoolingPolicy: "wait",
		MaxWait:          "15s",
		LogLevel:         "info",
		Cooldown: cooldownConfig{
			Disable:           false,
			Statuses:          []int{429},
			Base:              "30s",
			Factor:            2.0,
			Max:               "30m",
			Jitter:            0.2,
			TransientStatuses: []int{500, 502, 503, 504},
			TransientBase:     "15s",
			TransientFactor:   2.0,
			TransientMax:      "5m",
		},
	}
	// Populate parsed durations. These MUST stay consistent with the string
	// fields above (the single source of truth for YAML). defaultConfig is used
	// directly by tests and as the base for decodeConfig, so it must carry valid
	// parsed durations (validateConfig also fills them for the YAML path).
	cfg.ReloadIntervalDur = 2 * time.Second
	cfg.AttemptTimeoutDur = 120 * time.Second
	cfg.TotalTimeoutDur = 600 * time.Second
	cfg.MaxWaitDur = 15 * time.Second
	cfg.CooldownBase = 30 * time.Second
	cfg.CooldownMax = 30 * time.Minute
	cfg.CooldownTransientBase = 15 * time.Second
	cfg.CooldownTransientMax = 5 * time.Minute
	return cfg
}

// decodeConfig parses YAML onto a fresh default config and validates it.
func decodeConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if len(raw) > 0 {
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(false)
		if err := dec.Decode(&cfg); err != nil {
			return config{}, fmt.Errorf("配置错误: config_yaml: 解析失败: %w", err)
		}
	}

	// Normalize strings.
	cfg.NamePrefix = strings.TrimSpace(cfg.NamePrefix)
	cfg.AllCoolingPolicy = strings.TrimSpace(cfg.AllCoolingPolicy)
	cfg.LogLevel = strings.TrimSpace(cfg.LogLevel)

	if err := validateConfig(cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// parseDurationField parses a duration string and returns a field-named error
// when it is invalid or non-positive (unless allowZero is set).
func parseDurationField(field, s string, allowZero bool) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("配置错误: %s: 不是合法的时长: %q", field, s)
	}
	if d <= 0 && !allowZero {
		return 0, fmt.Errorf("配置错误: %s: 必须是正数时长", field)
	}
	return d, nil
}

// validateConfig enforces the rules documented in §4.
func validateConfig(cfg config) error {
	// Parse and validate duration fields with field-named errors.
	// reload_interval may be 0 (disabled) or positive.
	ri, err := parseDurationField("reload_interval", cfg.ReloadInterval, true)
	if err != nil {
		return err
	}
	if ri < 0 {
		return fmt.Errorf("配置错误: reload_interval: 必须是正数时长")
	}
	at, err := parseDurationField("attempt_timeout", cfg.AttemptTimeout, false)
	if err != nil {
		return err
	}
	tt, err := parseDurationField("total_timeout", cfg.TotalTimeout, false)
	if err != nil {
		return err
	}
	mw, err := parseDurationField("max_wait", cfg.MaxWait, false)
	if err != nil {
		return err
	}
	cb, err := parseDurationField("cooldown.base", cfg.Cooldown.Base, false)
	if err != nil {
		return err
	}
	cm, err := parseDurationField("cooldown.max", cfg.Cooldown.Max, false)
	if err != nil {
		return err
	}
	ctb, err := parseDurationField("cooldown.transient_base", cfg.Cooldown.TransientBase, false)
	if err != nil {
		return err
	}
	ctm, err := parseDurationField("cooldown.transient_max", cfg.Cooldown.TransientMax, false)
	if err != nil {
		return err
	}

	if cfg.MaxAttempts < 0 {
		return fmt.Errorf("配置错误: max_attempts: 必须 >= 0")
	}
	if cfg.Cooldown.Factor < 1 {
		return fmt.Errorf("配置错误: cooldown.factor: 必须 >= 1")
	}
	if cfg.Cooldown.Jitter < 0 || cfg.Cooldown.Jitter >= 1 {
		return fmt.Errorf("配置错误: cooldown.jitter: 必须在 [0,1) 区间")
	}
	if cfg.Cooldown.TransientFactor < 1 {
		return fmt.Errorf("配置错误: cooldown.transient_factor: 必须 >= 1")
	}
	switch cfg.AllCoolingPolicy {
	case "", "wait", "first", "error":
	default:
		return fmt.Errorf("配置错误: all_cooling_policy: 必须是 wait|first|error 之一")
	}
	switch cfg.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("配置错误: log_level: 必须是 debug|info|warn|error 之一")
	}
	if cm < cb {
		return fmt.Errorf("配置错误: cooldown.max: 必须 >= cooldown.base")
	}
	if ctm < ctb {
		return fmt.Errorf("配置错误: cooldown.transient_max: 必须 >= cooldown.transient_base")
	}
	for _, s := range cfg.Cooldown.Statuses {
		if s < 100 || s > 599 {
			return fmt.Errorf("配置错误: cooldown.statuses: 值 %d 必须在 100..599", s)
		}
	}
	for _, s := range cfg.Cooldown.TransientStatuses {
		if s < 100 || s > 599 {
			return fmt.Errorf("配置错误: cooldown.transient_statuses: 值 %d 必须在 100..599", s)
		}
	}
	if !isValidNamePrefix(cfg.NamePrefix) {
		return fmt.Errorf("配置错误: name_prefix: 只能包含 [A-Za-z0-9._:/-]")
	}

	// Populate parsed durations.
	cfg.ReloadIntervalDur = ri
	cfg.AttemptTimeoutDur = at
	cfg.TotalTimeoutDur = tt
	cfg.MaxWaitDur = mw
	cfg.CooldownBase = cb
	cfg.CooldownMax = cm
	cfg.CooldownTransientBase = ctb
	cfg.CooldownTransientMax = ctm
	return nil
}

// TransientJitter mirrors Jitter (no separate config field).
func (c cooldownConfig) TransientJitter() float64 { return c.Jitter }

// isValidNamePrefix checks [A-Za-z0-9._:/-] (empty allowed).
func isValidNamePrefix(p string) bool {
	for _, r := range p {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == ':' || r == '/' || r == '-':
		default:
			return false
		}
	}
	return true
}

// Package config loads and validates the service configuration.
package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/spf13/viper"
)

const (
	DefaultPollInterval               = time.Minute
	MinimumPollInterval               = 30 * time.Second
	DefaultHTTPTimeout                = 10 * time.Second
	DefaultFailureThreshold           = 3
	DefaultRecoveryConfirmationPeriod = 3 * time.Minute
	DefaultTimezone                   = "Europe/London"
	DefaultLogLevel                   = "info"
	DefaultLogFormat                  = "text"
)

// Config is the complete validated service configuration.
type Config struct {
	Telegram      TelegramConfig `mapstructure:"telegram" yaml:"telegram"`
	TfL           TfLConfig      `mapstructure:"tfl" yaml:"tfl"`
	DataDirectory string         `mapstructure:"data_directory" yaml:"data_directory"`
	Logging       LoggingConfig  `mapstructure:"logging" yaml:"logging"`
}

type TelegramConfig struct {
	Token        string  `mapstructure:"token" yaml:"token"`
	AllowedChats []int64 `mapstructure:"allowed_chats" yaml:"allowed_chats"`
}

type TfLConfig struct {
	AppKey        string             `mapstructure:"app_key" yaml:"app_key"`
	Monitor       MonitorConfig      `mapstructure:"monitor" yaml:"monitor"`
	Notifications NotificationConfig `mapstructure:"notifications" yaml:"notifications"`
}

type NotificationConfig struct {
	RecoveryConfirmationPeriod time.Duration `mapstructure:"recovery_confirmation_period" yaml:"recovery_confirmation_period"`
}

type MonitorConfig struct {
	PollInterval     time.Duration `mapstructure:"poll_interval" yaml:"poll_interval"`
	HTTPTimeout      time.Duration `mapstructure:"http_timeout" yaml:"http_timeout"`
	FailureThreshold int           `mapstructure:"failure_threshold" yaml:"failure_threshold"`
	DefaultTimezone  string        `mapstructure:"default_timezone" yaml:"default_timezone"`
}

type LoggingConfig struct {
	Level  string `mapstructure:"level" yaml:"level"`
	Format string `mapstructure:"format" yaml:"format"`
}

// LoadOptions controls one isolated configuration load.
type LoadOptions struct {
	ConfigPath string
	LogLevel   string
}

// Load creates a fresh Viper instance, loads the requested sources, and validates all fields.
func Load(opts LoadOptions) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix("EDI")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	defaults := map[string]any{
		"tfl.notifications.recovery_confirmation_period": DefaultRecoveryConfirmationPeriod.String(),
		"data_directory":                filepath.Join(xdg.DataHome, "edi"),
		"tfl.monitor.poll_interval":     DefaultPollInterval.String(),
		"tfl.monitor.http_timeout":      DefaultHTTPTimeout.String(),
		"tfl.monitor.failure_threshold": DefaultFailureThreshold,
		"tfl.monitor.default_timezone":  DefaultTimezone,
		"logging.level":                 DefaultLogLevel,
		"logging.format":                DefaultLogFormat,
	}
	for key, value := range defaults {
		v.SetDefault(key, value)
	}

	keys := []string{
		"telegram.token", "telegram.allowed_chats", "tfl.app_key", "data_directory",
		"tfl.monitor.poll_interval", "tfl.monitor.http_timeout", "tfl.monitor.failure_threshold",
		"tfl.monitor.default_timezone", "logging.level", "logging.format",
		"tfl.notifications.recovery_confirmation_period",
	}
	for _, key := range keys {
		if err := v.BindEnv(key); err != nil {
			return Config{}, fieldFailure{field: key, rule: "could not bind environment variable", err: fmt.Errorf("binding environment key %s: %w", key, err)}
		}
	}

	if opts.ConfigPath != "" {
		v.SetConfigFile(opts.ConfigPath)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fieldFailure{field: "config_file", rule: "could not read or parse YAML configuration", err: fmt.Errorf("reading config file %q: %w", opts.ConfigPath, err)}
		}
	}

	for _, key := range []string{"tfl.monitor.poll_interval", "tfl.monitor.http_timeout", "tfl.notifications.recovery_confirmation_period"} {
		if err := validateDurationValue(key, v.Get(key)); err != nil {
			return Config{}, err
		}
	}

	var parsedChats []int64
	if raw := v.Get("telegram.allowed_chats"); raw != nil {
		chats, parseErr := ParseAllowlist(raw)
		if parseErr != nil {
			return Config{}, fieldFailure{field: "telegram.allowed_chats", rule: "expected positive, unique numeric chat IDs", err: fmt.Errorf("telegram allowed chats: %w", parseErr)}
		}
		parsedChats = chats
		v.Set("telegram.allowed_chats", chats)
	}

	var cfg Config
	err := v.Unmarshal(&cfg)
	if err != nil {
		return Config{}, fieldFailure{field: "configuration", rule: "could not decode configuration", err: fmt.Errorf("decoding configuration: %w", err)}
	}
	cfg.Telegram.AllowedChats = parsedChats
	if opts.LogLevel != "" {
		cfg.Logging.Level = opts.LogLevel
	}

	if err := Validate(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateDurationValue(key string, value any) error {
	if value == nil {
		return nil
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return fieldFailure{field: key, rule: "expected a duration with units", err: fmt.Errorf("%s must use a duration with units", key)}
	case reflect.String:
		if _, err := time.ParseDuration(strings.TrimSpace(v.String())); err != nil {
			return fieldFailure{field: key, rule: "expected a duration with units", err: fmt.Errorf("%s must use a duration with units: %w", key, err)}
		}
		if raw := strings.TrimSpace(v.String()); raw == "0" || raw == "+0" || raw == "-0" {
			return fieldFailure{field: key, rule: "expected a duration with units", err: fmt.Errorf("%s must use a duration with units", key)}
		}
	}
	return nil
}

// ParseAllowlist accepts the documented comma-separated environment form and a YAML list.
func ParseAllowlist(value any) ([]int64, error) {
	var values []string
	switch typed := value.(type) {
	case string:
		values = strings.Split(typed, ",")
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	case []int64:
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	case []int:
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	default:
		return nil, fmt.Errorf("must be a comma-separated list of numeric chat IDs")
	}

	seen := make(map[int64]struct{}, len(values))
	chats := make([]int64, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("contains an empty chat ID")
		}
		var id int64
		if _, err := fmt.Sscan(value, &id); err != nil || id <= 0 || !strings.EqualFold(fmt.Sprint(id), value) {
			return nil, fmt.Errorf("contains invalid chat ID")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("contains duplicate chat ID")
		}
		seen[id] = struct{}{}
		chats = append(chats, id)
	}
	slices.Sort(chats)
	return chats, nil
}

// Validate reports all independent validation errors without exposing secret values.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fieldFailure{field: "configuration", rule: "is required", err: errors.New("configuration is nil")}
	}
	cfg.TfL.Monitor.DefaultTimezone = strings.TrimSpace(cfg.TfL.Monitor.DefaultTimezone)
	var errs []error
	add := func(field, rule string, err error) {
		errs = append(errs, fieldFailure{field: field, rule: rule, err: err})
	}
	if strings.TrimSpace(cfg.Telegram.Token) == "" {
		add("telegram.token", "is required", errors.New("telegram token is required"))
	}
	if len(cfg.Telegram.AllowedChats) == 0 {
		add("telegram.allowed_chats", "is required", errors.New("telegram allowed chats is required"))
	}
	seen := make(map[int64]struct{}, len(cfg.Telegram.AllowedChats))
	for _, id := range cfg.Telegram.AllowedChats {
		if id <= 0 {
			add("telegram.allowed_chats", "chat IDs must be positive", errors.New("telegram allowed chats must be positive"))
		}
		if _, ok := seen[id]; ok {
			add("telegram.allowed_chats", "chat IDs must be unique", errors.New("telegram allowed chats must not contain duplicates"))
		}
		seen[id] = struct{}{}
	}
	if strings.TrimSpace(cfg.DataDirectory) == "" {
		add("data_directory", "is required", errors.New("data directory is required"))
	}
	if cfg.TfL.Monitor.PollInterval < MinimumPollInterval {
		add("tfl.monitor.poll_interval", "must be at least 30 seconds", fmt.Errorf("monitor poll interval must be at least %s", MinimumPollInterval))
	}
	if cfg.TfL.Monitor.HTTPTimeout <= 0 {
		add("tfl.monitor.http_timeout", "must be positive", errors.New("monitor HTTP timeout must be positive"))
	}
	for field, duration := range map[string]time.Duration{
		"tfl.notifications.recovery_confirmation_period": cfg.TfL.Notifications.RecoveryConfirmationPeriod,
	} {
		if duration < 0 {
			add(field, "must not be negative", errors.New("monitor duration must not be negative"))
		}
	}
	if cfg.TfL.Monitor.FailureThreshold <= 0 {
		add("tfl.monitor.failure_threshold", "must be positive", errors.New("monitor failure threshold must be positive"))
	}
	if err := ValidateTimezone(cfg.TfL.Monitor.DefaultTimezone); err != nil {
		add("tfl.monitor.default_timezone", "expected a valid IANA timezone name", fmt.Errorf("monitor default timezone: %w", err))
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, strings.ToLower(cfg.Logging.Level)) {
		add("logging.level", "expected debug, info, warn, or error", errors.New("logging level must be debug, info, warn, or error"))
	}
	if !slices.Contains([]string{"json", "text"}, strings.ToLower(cfg.Logging.Format)) {
		add("logging.format", "expected json or text", errors.New("logging format must be json or text"))
	}
	return errors.Join(errs...)
}

// ValidateTimezone accepts IANA names and UTC, but not abbreviations or fixed offsets.
func ValidateTimezone(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("timezone is required")
	}
	if name != "UTC" && !strings.Contains(name, "/") {
		return errors.New("must be an IANA timezone name")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return errors.New("must be a valid IANA timezone name")
	}
	return nil
}

// fieldFailure keeps supplied values out of diagnostics before logging is configured.
type fieldFailure struct {
	field, rule string
	err         error
}

func (e fieldFailure) Error() string { return e.field + ": " + e.rule }
func (e fieldFailure) Unwrap() error { return e.err }

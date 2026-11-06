package config_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/motohedgehog/edi/internal/config"
)

func TestLoadEnvironmentOnly(t *testing.T) {
	t.Setenv("EDI_TELEGRAM_TOKEN", "token-value")
	t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", " 42, 7 ")
	t.Setenv("EDI_DATA_DIRECTORY", "")
	t.Setenv("EDI_LOGGING_FORMAT", "")
	cfg, err := config.Load(config.LoadOptions{})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Telegram.Token != "token-value" || len(cfg.Telegram.AllowedChats) != 2 || cfg.Telegram.AllowedChats[0] != 7 {
		t.Fatalf("unexpected Telegram config: %+v", cfg.Telegram)
	}
	if cfg.TfL.Monitor.PollInterval != time.Minute || cfg.TfL.Monitor.HTTPTimeout != config.DefaultHTTPTimeout || cfg.DataDirectory != filepath.Join(xdg.DataHome, "edi") {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Logging.Format != "text" {
		t.Fatalf("default logging format = %q, want text", cfg.Logging.Format)
	}
}

func TestLoadXDGDataDirectory(t *testing.T) {
	if want := os.Getenv("EDI_TEST_EXPECTED_DIRECTORY"); want != "" {
		cfg, err := config.Load(config.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DataDirectory != want {
			t.Fatalf("data directory = %q, want %q", cfg.DataDirectory, want)
		}
		return
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("directory defaults cover Linux and macOS")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, value string
		set         bool
	}{
		{name: "unset"},
		{name: "empty", set: true},
		{name: "relative", value: "relative-data", set: true},
		{name: "absolute", set: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			want := filepath.Join(home, ".local", "share", "edi")
			if runtime.GOOS == "darwin" {
				want = filepath.Join(home, "Library", "Application Support", "edi")
			}
			value := test.value
			if test.name == "absolute" {
				value = filepath.Join(home, "custom-data")
				want = filepath.Join(value, "edi")
			}
			cmd := exec.Command(executable, "-test.run=^TestLoadXDGDataDirectory$")
			for _, entry := range os.Environ() {
				key, _, _ := strings.Cut(entry, "=")
				if key != "HOME" && key != "XDG_DATA_HOME" && !strings.HasPrefix(key, "EDI_") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+home,
				"EDI_TELEGRAM_TOKEN=test-token", "EDI_TELEGRAM_ALLOWED_CHATS=42",
				"EDI_TEST_EXPECTED_DIRECTORY="+want)
			if test.set {
				cmd.Env = append(cmd.Env, "XDG_DATA_HOME="+value)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("subprocess failed: %v\n%s", err, output)
			}
		})
	}
}

func TestLoadDataDirectory(t *testing.T) {
	for _, test := range []struct {
		name, yaml, env, want string
	}{
		{name: "relative YAML directory", yaml: "./yaml-data", want: "./yaml-data"},
		{name: "environment overrides YAML", yaml: "./yaml-data", env: "/var/lib/edi", want: "/var/lib/edi"},
		{name: "whitespace YAML directory", yaml: "   "},
		{name: "whitespace environment directory", yaml: "./yaml-data", env: "   "},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("data_directory: \""+test.yaml+"\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("EDI_TELEGRAM_TOKEN", "test-token")
			t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "42")
			t.Setenv("EDI_DATA_DIRECTORY", test.env)
			cfg, err := config.Load(config.LoadOptions{ConfigPath: path})
			if test.want == "" {
				if err == nil {
					t.Fatal("Load() accepted a whitespace-only data directory")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.DataDirectory != test.want {
				t.Fatalf("data directory = %q, want %q", cfg.DataDirectory, test.want)
			}
		})
	}
}

func TestLoadPrecedenceAndDurationDecoding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte(`telegram:
  token: yaml-token
  allowed_chats: [42]
data_directory: ./yaml-data
tfl:
  monitor:
    poll_interval: 90s
    http_timeout: 5s
    failure_threshold: 4
logging:
  level: dEbUg
  format: text
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDI_TELEGRAM_TOKEN", "")
	t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "")
	t.Setenv("EDI_TFL_MONITOR_POLL_INTERVAL", "")
	t.Setenv("EDI_TFL_MONITOR_HTTP_TIMEOUT", "3s")
	t.Setenv("EDI_DATA_DIRECTORY", "./env-data")
	t.Setenv("EDI_LOGGING_LEVEL", "WaRn")

	cfg, err := config.Load(config.LoadOptions{ConfigPath: path, LogLevel: "eRrOr"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telegram.Token != "yaml-token" || cfg.TfL.Monitor.PollInterval != 90*time.Second || cfg.TfL.Monitor.HTTPTimeout != 3*time.Second {
		t.Fatalf("YAML or environment values decoded incorrectly: %+v", cfg)
	}
	if cfg.DataDirectory != "./env-data" || cfg.TfL.Monitor.FailureThreshold != 4 || cfg.Logging.Format != "text" || cfg.Logging.Level != "eRrOr" {
		t.Fatalf("configuration precedence is wrong: %+v", cfg)
	}
}

func TestLoadRejectsInvalidAllowlistAndDuration(t *testing.T) {
	t.Setenv("EDI_TELEGRAM_TOKEN", "secret")
	t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "1,1")
	t.Setenv("EDI_TFL_MONITOR_POLL_INTERVAL", "60")
	if _, err := config.Load(config.LoadOptions{}); err == nil {
		t.Fatal("Load() accepted invalid configuration")
	}
	if _, err := config.ParseAllowlist("1,abc"); err == nil {
		t.Fatal("ParseAllowlist() accepted nonnumeric ID")
	}
}

func TestLoadRejectsUnsupportedLoggingLevel(t *testing.T) {
	t.Setenv("EDI_TELEGRAM_TOKEN", "token")
	t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "42")
	t.Setenv("EDI_LOGGING_LEVEL", "trace")
	if _, err := config.Load(config.LoadOptions{}); err == nil {
		t.Fatal("Load() accepted an unsupported logging level")
	}
}

func TestValidateTimezone(t *testing.T) {
	if err := config.ValidateTimezone("Europe/London"); err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateTimezone("+01:00"); err == nil {
		t.Fatal("fixed offset accepted")
	}
	if err := config.ValidateTimezone("EST"); err == nil {
		t.Fatal("abbreviation accepted")
	}
}

func TestLoadAcceptsSharedConfigFilesAndNormalizesTimezone(t *testing.T) {
	for _, test := range []struct {
		name, extension, contents string
	}{
		{"yaml", "yaml", "telegram:\n  token: test-token\n  allowed_chats: [42]\n"},
		{"quoted yaml", "yaml", "telegram:\n  \"token\": test-token\n  allowed_chats: [42]\n"},
		{"compact yaml", "yaml", "telegram: {token: test-token, allowed_chats: [42]}\n"},
		{"json", "json", `{"telegram":{"token":"test-token","allowed_chats":[42]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config."+test.extension)
			if err := os.WriteFile(path, []byte(test.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("EDI_TFL_MONITOR_DEFAULT_TIMEZONE", " Europe/London ")
			cfg, err := config.Load(config.LoadOptions{ConfigPath: path})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TfL.Monitor.DefaultTimezone != "Europe/London" {
				t.Fatalf("timezone = %q", cfg.TfL.Monitor.DefaultTimezone)
			}
			if _, err := time.LoadLocation(cfg.TfL.Monitor.DefaultTimezone); err != nil {
				t.Fatalf("accepted timezone cannot be used: %v", err)
			}
		})
	}
}

func TestLoadErrorsHideSuppliedValues(t *testing.T) {
	for _, test := range []struct{ name, body, envKey, envValue, field string }{
		{"duration", "", "EDI_TFL_MONITOR_HTTP_TIMEOUT", "private-duration-marker", "tfl.monitor.http_timeout"},
		{"allowlist", "", "EDI_TELEGRAM_ALLOWED_CHATS", "private-chat-marker", "telegram.allowed_chats"},
		{"decode", "tfl:\n  monitor:\n    failure_threshold: private-number-marker\n", "", "", "configuration"},
		{"yaml", "telegram: [private-yaml-marker\n", "", "", "config_file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("EDI_TELEGRAM_TOKEN", "private-token-marker")
			t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "42")
			var opts config.LoadOptions
			if test.envKey != "" {
				t.Setenv(test.envKey, test.envValue)
			}
			if test.body != "" {
				opts.ConfigPath = filepath.Join(t.TempDir(), "private-path-marker.yaml")
				if err := os.WriteFile(opts.ConfigPath, []byte(test.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := config.Load(opts)
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("Load error = %v", err)
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatalf("configuration value leaked: %v", err)
			}
			if errors.Unwrap(err) == nil {
				t.Fatal("underlying cause lost")
			}
		})
	}
}

func TestMissingConfigErrorPreservesCauseWithoutPath(t *testing.T) {
	_, err := config.Load(config.LoadOptions{ConfigPath: filepath.Join(t.TempDir(), "private-path-marker.yaml")})
	if err == nil || strings.Contains(err.Error(), "private-") || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration file error = %v", err)
	}
}

func TestValidationErrorsAreJoinedAndSafe(t *testing.T) {
	cfg := config.Config{Telegram: config.TelegramConfig{Token: "private-token-marker", AllowedChats: []int64{42, 42}}, Logging: config.LoggingConfig{Level: "private-level-marker", Format: "private-format-marker"}, TfL: config.TfLConfig{Monitor: config.MonitorConfig{DefaultTimezone: "private-zone-marker"}}}
	err := config.Validate(&cfg)
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatalf("validation error = %v", err)
	}
	for _, field := range []string{"telegram.allowed_chats", "logging.level", "logging.format", "tfl.monitor.default_timezone"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing field %s", field)
		}
	}
	if _, ok := err.(interface{ Unwrap() []error }); !ok {
		t.Fatal("validation causes are not joined")
	}
}

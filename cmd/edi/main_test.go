package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motohedgehog/edi/internal/app"
	"github.com/motohedgehog/edi/internal/config"
	"github.com/motohedgehog/edi/internal/logging/logtest"
)

func TestStartupFailurePreservesFilesystemCause(t *testing.T) {
	logger, logs := logtest.New(slog.LevelDebug)
	path := filepath.Join(t.TempDir(), "private-username-marker", "private-directory-marker")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := app.New(t.Context(), config.Config{DataDirectory: path}, logger)
	if err == nil {
		t.Fatal("startup accepted a regular file as its data directory")
	}
	if _, ok := errors.AsType[*os.PathError](err); !ok {
		t.Fatal("startup error lost its filesystem cause")
	}
	logFailure(logger, runFailure{operation: "startup", err: err})
	records := logs.Records(t)
	if !strings.Contains(records[len(records)-1]["error"].(string), path) {
		t.Fatal("filesystem path missing from diagnostic")
	}
}

func TestConfiguredLoggerRedactsActiveCredentials(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			previous := os.Stderr
			os.Stderr = writer
			t.Cleanup(func() { os.Stderr = previous })
			cfg := config.Config{Telegram: config.TelegramConfig{Token: "123:telegram-token"}, TfL: config.TfLConfig{AppKey: "tfl-key"}, Logging: config.LoggingConfig{Level: "info", Format: format}}
			logger, err := newLogger(cfg)
			if err != nil {
				t.Fatal(err)
			}
			logger.Error("Request failed", "error", errors.New("Post https://telegram/bot123:telegram-token?app_key=tfl-key: connection refused"))
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			description := string(data)
			if format == "json" {
				var record map[string]any
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				description = record["error"].(string)
			}
			if !strings.Contains(description, "<redacted>") || !strings.Contains(description, "connection refused") || strings.Contains(description, cfg.Telegram.Token) || strings.Contains(description, cfg.TfL.AppKey) {
				t.Fatalf("configured logger output = %s", description)
			}
		})
	}
}

func TestRunInvalidConfigurationStopsBeforeStartup(t *testing.T) {
	previousArgs, previousFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = previousArgs, previousFlags })
	os.Args = []string{"edi"}
	flag.CommandLine = flag.NewFlagSet("edi-test", flag.ContinueOnError)
	dir := filepath.Join(t.TempDir(), "data")
	t.Setenv("EDI_TELEGRAM_TOKEN", "secret-token")
	t.Setenv("EDI_TELEGRAM_ALLOWED_CHATS", "42")
	t.Setenv("EDI_DATA_DIRECTORY", dir)
	t.Setenv("EDI_TFL_MONITOR_HTTP_TIMEOUT", "private-invalid-duration")
	err := run()
	if err == nil || strings.Contains(err.Error(), "private-invalid-duration") || !strings.Contains(err.Error(), "tfl.monitor.http_timeout") {
		t.Fatalf("configuration error = %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid configuration started application: %v", err)
	}
}

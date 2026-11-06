package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/motohedgehog/edi/internal/app"
	"github.com/motohedgehog/edi/internal/config"
	"github.com/motohedgehog/edi/internal/logging"
)

func main() {
	if err := run(); err != nil {
		logFailure(slog.Default(), err)
		os.Exit(1)
	}
}

func logFailure(logger *slog.Logger, err error) {
	operation := "configuration"
	if failure, ok := errors.AsType[runFailure](err); ok {
		operation = failure.operation
	}
	message := "Application configuration failed"
	switch operation {
	case "startup":
		message = "Application startup failed"
	case "runtime":
		message = "Application runtime failed"
	}
	attrs := []slog.Attr{logging.Error(err)}
	var startup interface{ StartupStage() string }
	if errors.As(err, &startup) {
		attrs = append(attrs, slog.String("stage", startup.StartupStage()))
	}
	var worker interface{ WorkerName() string }
	if errors.As(err, &worker) && worker.WorkerName() != "" {
		attrs = append(attrs, slog.String("worker", worker.WorkerName()))
	}
	logger.LogAttrs(context.Background(), slog.LevelError, message, attrs...)
}

func run() error {
	configPath := flag.String("config", "", "path to a YAML configuration file")
	logLevel := flag.String("log-level", "", "override the configured log level")
	showVersion := flag.Bool("version", false, "print build information and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(app.VersionString())
		return nil
	}
	cfg, err := config.Load(config.LoadOptions{ConfigPath: *configPath, LogLevel: *logLevel})
	if err != nil {
		return err
	}
	logger, err := newLogger(cfg)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return runFailure{operation: "startup", err: err}
	}
	defer func() {
		if closeErr := application.Close(); closeErr != nil {
			logger.Error("Application database close failed", logging.Error(closeErr))
		}
	}()
	logger.Info("Application ready")
	err = application.Run(ctx)
	if err != nil {
		return runFailure{operation: "runtime", err: err}
	}
	logger.Info("Application stopped")
	return nil
}

func newLogger(cfg config.Config) (*slog.Logger, error) {
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
		return nil, errors.New("invalid log level")
	}
	opts := &slog.HandlerOptions{Level: slogLevel, ReplaceAttr: logging.RedactCredentials(cfg.Telegram.Token, cfg.TfL.AppKey)}
	if strings.EqualFold(cfg.Logging.Format, "text") {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
}

type runFailure struct {
	operation string
	err       error
}

func (e runFailure) Error() string { return e.err.Error() }
func (e runFailure) Unwrap() error { return e.err }

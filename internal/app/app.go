// Package app owns dependency construction and the application lifecycle.
package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/motohedgehog/edi/internal/config"
	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/telegram"
	"github.com/motohedgehog/edi/internal/tfl"
)

const referenceRefreshInterval = 24 * time.Hour

type App struct {
	logger    *slog.Logger
	store     *store.Store
	bot       *telegram.Bot
	monitor   *monitor.Service
	refresh   func(context.Context) error
	pollEvery time.Duration
	closeOnce sync.Once
}

// New performs the startup sequence. It does not start goroutines.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "Application starting", "version", Version,
		"log_level", strings.ToLower(cfg.Logging.Level), "log_format", strings.ToLower(cfg.Logging.Format))
	database, err := store.Open(ctx, filepath.Join(cfg.DataDirectory, "edi.db"))
	if err != nil {
		return nil, startupFailure{stage: "database_initialization", err: err}
	}
	cleanup := func(startErr error) (*App, error) {
		_ = database.Close()
		return nil, startErr
	}

	status := tfl.NewClient(cfg.TfL.AppKey, cfg.TfL.Monitor.HTTPTimeout, tfl.WithVersion(Version), tfl.WithLogger(logger))
	referenceData := newReferences(database, status, logger)
	_ = referenceData.Initialize(ctx)

	checker := &manualChecker{client: status, catalog: referenceData.Catalog, clock: monitor.RealClock{}}
	router := telegram.NewRouter(database, telegram.RouterOptions{AllowedUsers: cfg.Telegram.AllowedChats,
		Logger: logger, DefaultZone: cfg.TfL.Monitor.DefaultTimezone, Catalog: referenceData.Catalog, StatusChecker: checker})
	bot, err := telegram.NewBot(ctx, cfg.Telegram.Token, router, nil, logger)
	if err != nil {
		return cleanup(startupFailure{stage: "telegram_initialization", err: err})
	}
	monitorService := monitor.NewService(database, status, bot, monitor.ServiceOptions{Catalog: referenceData.Catalog,
		Logger: logger, AllowedUsers: cfg.Telegram.AllowedChats, FailureThreshold: cfg.TfL.Monitor.FailureThreshold,
		RecoveryConfirmationPeriod: cfg.TfL.Notifications.RecoveryConfirmationPeriod, AlertCoalescingWindow: cfg.TfL.Notifications.AlertCoalescingWindow})
	return &App{logger: logger, store: database, bot: bot, monitor: monitorService,
		refresh: referenceData.Refresh, pollEvery: cfg.TfL.Monitor.PollInterval}, nil
}

type startupFailure struct {
	stage string
	err   error
}

func (e startupFailure) Error() string        { return e.err.Error() }
func (e startupFailure) Unwrap() error        { return e.err }
func (e startupFailure) StartupStage() string { return e.stage }

// Run starts the Telegram intake, monitor loop, and reference refresh work.
func (a *App) Run(ctx context.Context) error {
	if a == nil {
		return errors.New("application is nil")
	}
	if a.pollEvery <= 0 {
		a.pollEvery = time.Minute
	}
	return runWorkers(ctx, a.logger,
		ownedWorker{name: "telegram", run: a.bot.Run},
		ownedWorker{name: "monitor", run: a.monitorLoop},
		ownedWorker{name: "reference-refresh", run: a.referenceLoop},
	)
}

func (a *App) referenceLoop(ctx context.Context) error {
	ticker := time.NewTicker(referenceRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = a.refresh(ctx)
		}
	}
}

func (a *App) monitorLoop(ctx context.Context) error {
	_, _ = a.monitor.Cycle(ctx)
	ticker := time.NewTicker(a.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := a.monitor.Cycle(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
			}
		}
	}
}

func (a *App) Close() error {
	var err error
	a.closeOnce.Do(func() {
		err = a.store.Close()
	})
	return err
}

type manualChecker struct {
	client  *tfl.Client
	catalog func() []tfl.Line
	clock   monitor.Clock
}

func (m *manualChecker) Check(ctx context.Context, selected []string, timezone string) ([]message.Chunk, error) {
	snapshot, err := m.client.Status(ctx)
	if err != nil {
		return nil, err
	}
	var catalog []tfl.Line
	if m.catalog != nil {
		catalog = m.catalog()
	}
	lines := tfl.ForSelection(snapshot, selected, catalog)
	ids := make([]string, 0, len(lines))
	for id := range lines {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	observedAt := snapshot.ObservedAt
	if observedAt.IsZero() {
		observedAt = m.clock.Now()
	}
	alertLines := make([]monitor.AlertLine, 0, len(ids))
	for _, id := range ids {
		line := lines[id]
		alertLines = append(alertLines, monitor.AlertLine{LineID: id, DisplayName: line.DisplayName, Statuses: line.Statuses})
	}
	return monitor.RenderStatusCheck(alertLines, observedAt, timezone), nil
}

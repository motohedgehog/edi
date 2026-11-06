package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/telegram"
	"github.com/motohedgehog/edi/internal/tfl"
)

func TestRunWorkersCancelsAndJoinsEveryWorker(t *testing.T) {
	started := make(chan struct{}, 2)
	finished := make(chan struct{}, 2)
	worker := func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		finished <- struct{}{}
		return nil
	}
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runWorkers(parent, slog.New(slog.NewTextHandler(io.Discard, nil)),
			ownedWorker{name: "telegram", run: worker}, ownedWorker{name: "monitor", run: worker})
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWorkers error = %v, want nil for parent cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWorkers did not join canceled workers")
	}
	for range 2 {
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("worker returned before all work was joined")
		}
	}
}

func TestRunWorkersPreservesFirstSubstantiveErrorAndJoinsPeers(t *testing.T) {
	wantErr := errors.New("monitor operation failed")
	monitorReady := make(chan struct{})
	releaseMonitor := make(chan struct{})
	telegramStopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runWorkers(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)),
			ownedWorker{name: "telegram", run: func(ctx context.Context) error {
				<-ctx.Done()
				close(telegramStopped)
				return ctx.Err()
			}},
			ownedWorker{name: "monitor", run: func(context.Context) error {
				close(monitorReady)
				<-releaseMonitor
				return wantErr
			}},
		)
	}()
	<-monitorReady
	close(releaseMonitor)
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("RunWorkers error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("RunWorkers did not cancel and join peers")
	}
	select {
	case <-telegramStopped:
	default:
		t.Fatal("peer worker was not joined")
	}
}

func TestRunWorkersRedactsPanicAndUnexpectedReturn(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(context.Context) error
		want string
	}{
		{name: "panic", run: func(context.Context) error { panic("private panic payload") }, want: "panicked"},
		{name: "unexpected return", run: func(context.Context) error { return nil }, want: "stopped unexpectedly"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := runWorkers(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), ownedWorker{name: "telegram", run: test.run})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("RunWorkers error = %v, want %q", err, test.want)
			}
			if strings.Contains(err.Error(), "private panic payload") {
				t.Fatalf("panic payload leaked: %v", err)
			}
		})
	}
}

type barrierStatusSource struct {
	started chan struct{}
	release chan struct{}
}

func (s barrierStatusSource) Status(context.Context) (tfl.Snapshot, error) {
	close(s.started)
	<-s.release
	return tfl.Snapshot{ObservedAt: time.Now().UTC()}, nil
}

func TestAppRunKeepsDatabaseLockUntilOwnedWorkFinishes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "edi.db")
	database, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := database.SaveProfile(ctx, store.Profile{
		UserID: 42, PersonaID: store.PersonaTFL, Enabled: true, AllDay: true,
		Timezone: "Europe/London", Lines: []string{"central"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	botClient := &http.Client{Timeout: time.Second, Transport: appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/getUpdates") {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		body := `{"ok":true,"result":true}`
		if strings.HasSuffix(request.URL.Path, "/getMe") {
			body = `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"test_bot"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	router := telegram.NewRouter(database, telegram.RouterOptions{AllowedUsers: []int64{profile.UserID}, Catalog: func() []tfl.Line {
		return []tfl.Line{{ID: "central", DisplayName: "Central"}}
	}})
	bot, err := telegram.NewBot(ctx, "test-token", router, botClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	status := barrierStatusSource{started: make(chan struct{}), release: make(chan struct{})}
	service := monitor.NewService(database, status, nil, monitor.ServiceOptions{
		Catalog:      func() []tfl.Line { return []tfl.Line{{ID: "central", DisplayName: "Central"}} },
		AllowedUsers: []int64{profile.UserID},
	})
	application := &App{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), store: database, bot: bot, monitor: service,
		refresh: func(context.Context) error { return nil }, pollEvery: time.Hour,
	}
	runCtx, cancel := context.WithCancel(ctx)
	runDone := make(chan error, 1)
	go func() { runDone <- application.Run(runCtx) }()
	select {
	case <-status.started:
	case <-time.After(time.Second):
		cancel()
		close(status.release)
		t.Fatal("monitor worker did not enter its status request")
	}
	cancel()
	if _, err := store.Open(ctx, path); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("database lock while worker was still active = %v, want locked", err)
	}
	select {
	case err := <-runDone:
		t.Fatalf("application Run returned before active worker finished: %v", err)
	default:
	}
	close(status.release)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("application Run after cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("application did not join the monitor worker")
	}
	if err := application.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("database lock remained after joined Run and Close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

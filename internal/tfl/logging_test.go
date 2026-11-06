package tfl_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/logging/logtest"
	"github.com/motohedgehog/edi/internal/tfl"
)

func TestClientLogsRequestErrorsWithCredentialsRedacted(t *testing.T) {
	_, logs := logtest.New(slog.LevelDebug)
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: logging.RedactCredentials("", "private-key-marker")}))
	failing := true
	calls := 0
	client := tfl.NewClient("private-key-marker", time.Second, tfl.WithLogger(logger),
		tfl.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if failing {
				return nil, errors.New("private-error-marker " + request.URL.String())
			}
			return response(http.StatusOK, `[{"id":"central","modeName":"tube","lineStatuses":[{"statusSeverity":10,"statusSeverityDescription":"private-status-marker","reason":"private-reason-marker"}]},{}]`, "application/json"), nil
		})}), tfl.WithSleep(func(context.Context, time.Duration) error { return nil }),
		tfl.WithJitter(func(time.Duration) time.Duration { return 0 }))
	if _, err := client.Status(t.Context()); err == nil {
		t.Fatal("failed request succeeded")
	}
	failing = false
	for range 2 {
		if snapshot, err := client.Status(t.Context()); err != nil || !snapshot.Partial {
			t.Fatalf("snapshot=%+v err=%v", snapshot, err)
		}
	}
	if calls != 5 {
		t.Fatalf("calls=%d, want 5", calls)
	}
	logs.AssertPrivate(t, "private-key-marker", "private-status-marker", "private-reason-marker")
	if !strings.Contains(logs.String(), "private-error-marker") || !strings.Contains(logs.String(), "https://") || !strings.Contains(logs.String(), "app_key") {
		t.Fatal("request error details were lost")
	}
}

func TestClientFailureDoesNotExposePayload(t *testing.T) {
	for _, canceled := range []bool{true, false} {
		t.Run(map[bool]string{true: "canceled", false: "forbidden"}[canceled], func(t *testing.T) {
			logger, logs := logtest.New(slog.LevelDebug)
			client := tfl.NewClient("private-key", time.Second, tfl.WithLogger(logger), tfl.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(http.StatusForbidden, "private-payload", "text/html"), nil
			})}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			if _, err := client.Status(ctx); err == nil {
				t.Fatal("expected failure")
			}
			logs.AssertPrivate(t, "private-key", "private-payload")
		})
	}
}

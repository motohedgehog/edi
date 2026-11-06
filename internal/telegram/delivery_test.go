package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/monitor"
)

func TestAutomaticEditClassifiesTelegramResponses(t *testing.T) {
	for _, test := range []struct {
		name, response  string
		target, success bool
		delay           time.Duration
	}{
		{"deleted", `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`, true, false, 0},
		{"uneditable", `{"ok":false,"error_code":400,"description":"Bad Request: message can't be edited"}`, true, false, 0},
		{"unchanged", `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`, false, true, 0},
		{"format", `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`, false, false, 0},
		{"access", `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`, false, false, 0},
		{"retry", `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 17","parameters":{"retry_after":17}}`, false, false, 17 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/editMessageText") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(test.response)), Header: http.Header{}, Request: r}, nil
				}
				return testTelegramResponse(r), nil
			})
			bot.ready = true
			bot.runtimeCtx = context.Background()
			err := bot.Edit(context.Background(), 42, 123, "text", message.FormatHTML)
			if (err == nil) != test.success || errors.Is(err, monitor.ErrEditTargetUnavailable) != test.target {
				t.Fatalf("error=%v", err)
			}
			var retry monitor.RetryDelay
			if test.delay > 0 && (!errors.As(err, &retry) || retry.RetryDelay() != test.delay) {
				t.Fatalf("retry=%v", err)
			}
		})
	}
}

func TestAutomaticDeliveryHonorsOperationCancellation(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "edit"}[edit], func(t *testing.T) {
			started := make(chan struct{})
			bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/sendMessage") || strings.HasSuffix(r.URL.Path, "/editMessageText") {
					close(started)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return testTelegramResponse(r), nil
			})
			bot.ready = true
			bot.runtimeCtx = context.Background()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if edit {
					done <- bot.Edit(ctx, 42, 1, "text", message.FormatHTML)
				} else {
					_, err := bot.Send(ctx, 42, "text", message.FormatHTML)
					done <- err
				}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("operation did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("operation was not canceled")
			}
		})
	}
}

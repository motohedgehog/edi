package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/logging/logtest"
	tele "gopkg.in/telebot.v4"
)

func TestBotPollingHonorsTelegramRetryDelay(t *testing.T) {
	for _, seconds := range []int{30, 90} {
		t.Run(fmt.Sprintf("%d seconds", seconds), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				calls := make(chan time.Time, 2)
				count := 0
				bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(request.URL.Path, "/getUpdates") {
						return testTelegramResponse(request), nil
					}
					count++
					calls <- time.Now()
					if count > 1 {
						<-request.Context().Done()
						return nil, request.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Request: request,
						Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":%d}}`, seconds)))}, nil
				})
				done := make(chan error, 1)
				go func() { done <- bot.Run(ctx) }()
				defer func() {
					cancel()
					if err := <-done; err != nil {
						t.Errorf("shutdown: %v", err)
					}
				}()
				first := <-calls
				delay := time.Duration(seconds) * time.Second
				time.Sleep(delay - time.Nanosecond)
				synctest.Wait()
				select {
				case at := <-calls:
					t.Fatalf("retried after %v, before Telegram's %v delay", at.Sub(first), delay)
				default:
				}
				time.Sleep(time.Nanosecond)
				if at := <-calls; at.Sub(first) != delay {
					t.Fatalf("retry delay = %v, want %v", at.Sub(first), delay)
				}
			})
		})
	}
}

func TestBotPollingBacksOffMalformedResponsesAndResetsAfterSuccess(t *testing.T) {
	_, logs := logtest.New(slog.LevelDebug)
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug, ReplaceAttr: logging.RedactCredentials("test-token", "")}))
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		bodies := []string{
			"network error", "sensitive malformed response", `{"ok":true}`, `{"ok":true,"result":null}`,
			`{"ok":true,"result":{}}`, `{"ok":false,"error_code":500,"description":"sensitive API error"}`,
			`{"ok":true,"result":[]}`, "network error",
		}
		calls := make(chan time.Time, len(bodies)+1)
		count := 0
		bot := newLifecycleBotWithLogger(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(request.URL.Path, "/getUpdates") {
				return testTelegramResponse(request), nil
			}
			index := count
			count++
			calls <- time.Now()
			if index == len(bodies) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			if bodies[index] == "network error" {
				return nil, errors.New("sensitive network error")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
				Body: io.NopCloser(strings.NewReader(bodies[index]))}, nil
		}, logger)
		done := make(chan error, 1)
		go func() { done <- bot.Run(ctx) }()
		defer func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("shutdown: %v", err)
			}
		}()
		previous := <-calls
		for _, seconds := range []int{1, 2, 4, 8, 16, 30, 0, 1} {
			delay := time.Duration(seconds) * time.Second
			if delay > 0 {
				time.Sleep(delay - time.Nanosecond)
				synctest.Wait()
				select {
				case at := <-calls:
					t.Fatalf("retry after %v, want %v", at.Sub(previous), delay)
				default:
				}
				time.Sleep(time.Nanosecond)
			}
			at := <-calls
			if at.Sub(previous) != delay {
				t.Fatalf("retry delay = %v, want %v", at.Sub(previous), delay)
			}
			previous = at
		}
	})
	logs.AssertPrivate(t, "test-token", "sensitive malformed response", "test_bot")
	for _, description := range []string{"sensitive network error", "sensitive API error"} {
		if !strings.Contains(logs.String(), description) {
			t.Errorf("poll error missing %q", description)
		}
	}
}

func TestBotPollingShutdownInterruptsRequestAndRetryWait(t *testing.T) {
	for _, waitingFor := range []string{"request", "retry"} {
		t.Run(waitingFor, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				started := make(chan struct{})
				bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(request.URL.Path, "/getUpdates") {
						return testTelegramResponse(request), nil
					}
					close(started)
					if waitingFor == "request" {
						<-request.Context().Done()
						return nil, request.Context().Err()
					}
					return nil, errors.New("temporary")
				})
				done := make(chan error, 1)
				go func() { done <- bot.Run(ctx) }()
				<-started
				synctest.Wait()
				before := time.Now()
				cancel()
				if err := <-done; err != nil || !time.Now().Equal(before) {
					t.Fatalf("shutdown waited: elapsed=%v err=%v", time.Since(before), err)
				}
			})
		})
	}
}

func TestBotPollingAdvancesOffsetAfterQueuingUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := make(chan map[string]string, 2)
	count := 0
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(request.URL.Path, "/getUpdates") {
			return testTelegramResponse(request), nil
		}
		var params map[string]string
		if err := json.NewDecoder(request.Body).Decode(&params); err != nil {
			return nil, err
		}
		count++
		calls <- params
		if count > 1 {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
			Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":41},{"update_id":42}]}`))}, nil
	})
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	for _, want := range []string{"1", "43"} {
		if params := <-calls; params["offset"] != want || params["timeout"] != "10" {
			t.Fatalf("poll params=%v, want offset %s and 10-second timeout", params, want)
		}
	}
}

func TestPollingStopInterruptsBlockedUpdateDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
			if !strings.HasSuffix(request.URL.Path, "/getUpdates") {
				return testTelegramResponse(request), nil
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request,
				Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":41}]}`))}, nil
		})
		updates, stop, done := make(chan tele.Update), make(chan struct{}), make(chan struct{})
		go func() {
			bot.bot.Poller.Poll(bot.bot, updates, stop)
			close(done)
		}()
		synctest.Wait()
		close(stop)
		<-done
	})
}

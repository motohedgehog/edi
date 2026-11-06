package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/monitor"
	tele "gopkg.in/telebot.v4"
)

func TestBotRunPublishesCommandMenu(t *testing.T) {
	var payload struct {
		Commands []tele.Command `json:"commands"`
	}
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/setMyCommands") {
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatalf("decode command registration: %v", err)
			}
		}
		return testTelegramResponse(request), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	bot.bot.Poller = lifecyclePollerFunc(func(_ *tele.Bot, _ chan tele.Update, stop chan struct{}) {
		cancel()
		<-stop
	})
	if err := bot.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []tele.Command{
		{Text: "tfl", Description: "Configure TfL alerts"},
		{Text: "help", Description: "Show help and available commands"},
	}
	if !slices.Equal(payload.Commands, want) {
		t.Fatalf("published commands = %+v, want %+v", payload.Commands, want)
	}
}

func TestDeliveryErrorPreservesUpstreamDescriptionAndCause(t *testing.T) {
	for _, cause := range []error{
		&url.Error{Op: "Post", URL: "https://api.telegram.org/bottoken/sendMessage", Err: errors.New("connection refused")},
		&tele.Error{Code: 400, Description: "Bad Request: message is too long"},
		tele.FloodError{RetryAfter: 30},
		errors.New("ordinary polling failure"),
	} {
		original := fmt.Errorf("telegram request: %w", cause)
		got := safeDeliveryError(t.Context(), original, false)
		if got == nil || got.Error() != original.Error() || !errors.Is(got, original) {
			t.Fatalf("delivery error = %v, want %v with cause", got, original)
		}
		if flood, ok := errors.AsType[tele.FloodError](cause); ok {
			delay, ok := errors.AsType[interface {
				error
				RetryDelay() time.Duration
			}](got)
			if !ok || delay.RetryDelay() != time.Duration(flood.RetryAfter)*time.Second {
				t.Fatalf("retry delay lost: %v", got)
			}
		}
	}
}

func TestDeliveryEditSemanticsPreserveCause(t *testing.T) {
	for _, cause := range []error{
		errors.New("telegram: Bad Request: message to edit not found (400)"),
		tele.ErrCantEditMessage,
		&tele.Error{Code: 400, Description: "Bad Request: message to edit not found"},
	} {
		got := safeDeliveryError(t.Context(), cause, true)
		if got == nil || got.Error() != cause.Error() || !errors.Is(got, monitor.ErrEditTargetUnavailable) || !errors.Is(got, cause) {
			t.Fatalf("edit failure = %v", got)
		}
	}
	for _, cause := range []error{tele.ErrMessageNotModified, tele.ErrSameMessageContent} {
		if got := safeDeliveryError(t.Context(), fmt.Errorf("edit: %w", cause), true); got != nil {
			t.Fatalf("unchanged message = %v", got)
		}
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := safeDeliveryError(t.Context(), fmt.Errorf("request: %w", cause), false); !errors.Is(got, cause) {
			t.Fatalf("context failure = %v", got)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := safeDeliveryError(ctx, errors.New("transport failed"), false); !errors.Is(got, context.Canceled) {
		t.Fatalf("canceled request = %v", got)
	}
}

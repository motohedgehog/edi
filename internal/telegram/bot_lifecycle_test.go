package telegram

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/tfl"
	tele "gopkg.in/telebot.v4"
)

type lifecycleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f lifecycleRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type lifecyclePollerFunc func(*tele.Bot, chan tele.Update, chan struct{})

func (f lifecyclePollerFunc) Poll(bot *tele.Bot, updates chan tele.Update, stop chan struct{}) {
	f(bot, updates, stop)
}

type botStatusCheckFunc func(context.Context, []string, string) ([]message.Chunk, error)

func (f botStatusCheckFunc) Check(ctx context.Context, lines []string, timezone string) ([]message.Chunk, error) {
	return f(ctx, lines, timezone)
}

type botRequestLog struct {
	mu    sync.Mutex
	paths []string
	seen  chan string
}

func (r *botRequestLog) add(path string) {
	r.mu.Lock()
	r.paths = append(r.paths, path)
	r.mu.Unlock()
	if r.seen != nil {
		select {
		case r.seen <- path:
		default:
		}
	}
}

func (r *botRequestLog) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func testTelegramResponse(request *http.Request) *http.Response {
	body := `{"ok":true,"result":true}`
	if strings.HasSuffix(request.URL.Path, "/getMe") {
		body = `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test","username":"test_bot"}}`
	}
	if strings.HasSuffix(request.URL.Path, "/sendMessage") || strings.HasSuffix(request.URL.Path, "/editMessageText") {
		body = `{"ok":true,"result":{"message_id":1,"date":1,"chat":{"id":42,"type":"private"}}}`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func newLifecycleBot(t *testing.T, router *Router, roundTrip lifecycleRoundTripFunc) *Bot {
	t.Helper()
	return newLifecycleBotWithLogger(t, router, roundTrip, nil)
}

func newLifecycleBotWithLogger(t *testing.T, router *Router, roundTrip lifecycleRoundTripFunc, logger *slog.Logger) *Bot {
	t.Helper()
	bot, err := NewBot(context.Background(), "test-token", router, &http.Client{Timeout: 5 * time.Second, Transport: roundTrip}, logger)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	return bot
}

func callbackDataForCheck(t *testing.T, router *Router) string {
	t.Helper()
	ctx := context.Background()
	menu, err := router.Handle(ctx, Update{UserID: 42, ChatType: PrivateChat, Command: CommandRoot})
	if err != nil || len(menu) != 1 {
		t.Fatalf("open router menu: actions=%+v err=%v", menu, err)
	}
	lines, err := router.Handle(ctx, Update{UserID: 42, ChatType: PrivateChat, CallbackData: buttonData(menu[0].Keyboard, "Choose lines")})
	if err != nil || len(lines) != 1 {
		t.Fatalf("open line menu: actions=%+v err=%v", lines, err)
	}
	lines, err = router.Handle(ctx, Update{UserID: 42, ChatType: PrivateChat, CallbackData: buttonData(lines[0].Keyboard, "Central")})
	if err != nil || len(lines) != 1 {
		t.Fatalf("select line: actions=%+v err=%v", lines, err)
	}
	return buttonData(lines[0].Keyboard, "Check selected lines")
}

func buttonData(keyboard [][]Button, text string) string {
	for _, row := range keyboard {
		for _, button := range row {
			if button.Text == text || strings.Contains(button.Text, text) || strings.TrimSuffix(button.Text, " ✓") == text {
				return button.Data
			}
		}
	}
	return ""
}

func callbackUpdate(data string) tele.Update {
	return tele.Update{Callback: &tele.Callback{ID: "callback-id", Sender: &tele.User{ID: 42}, Data: data,
		Message: &tele.Message{ID: 10, Chat: &tele.Chat{ID: 42, Type: tele.ChatPrivate}}}}
}

func TestBotCallbackIsCanceledAndJoinedBeforeRunReturns(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	checker := botStatusCheckFunc(func(ctx context.Context, _ []string, _ string) ([]message.Chunk, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return nil, ctx.Err()
	})
	router := NewRouter(nil, RouterOptions{AllowedUsers: []int64{42}, Catalog: func() []tfl.Line {
		return []tfl.Line{{ID: "central", DisplayName: "Central"}}
	}, StatusChecker: checker})
	callbackData := callbackDataForCheck(t, router)
	requests := &botRequestLog{}
	bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
		requests.add(request.URL.Path)
		return testTelegramResponse(request), nil
	})
	updateSent := make(chan struct{})
	bot.bot.Poller = lifecyclePollerFunc(func(_ *tele.Bot, updates chan tele.Update, stop chan struct{}) {
		updates <- callbackUpdate(callbackData)
		close(updateSent)
		<-stop
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	<-updateSent
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("callback handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Bot.Run error after parent cancellation: %v", err)
		}
		select {
		case <-finished:
		default:
			t.Fatal("Bot.Run returned before its callback completed")
		}
	case <-time.After(time.Second):
		t.Fatal("Bot.Run did not finish after callback cancellation")
	}
	for _, path := range requests.snapshot() {
		if strings.HasSuffix(path, "/sendMessage") || strings.HasSuffix(path, "/answerCallbackQuery") {
			t.Fatalf("callback sent a reply after shutdown began: %s", path)
		}
	}
}

func TestBotCallbackSendsAndAcknowledgesBeforeShutdown(t *testing.T) {
	checked := make(chan struct{})
	checker := botStatusCheckFunc(func(context.Context, []string, string) ([]message.Chunk, error) {
		close(checked)
		return []message.Chunk{{Body: "status", Format: message.FormatPlainText}}, nil
	})
	router := NewRouter(nil, RouterOptions{AllowedUsers: []int64{42}, Catalog: func() []tfl.Line {
		return []tfl.Line{{ID: "central", DisplayName: "Central"}}
	}, StatusChecker: checker})
	callbackData := callbackDataForCheck(t, router)
	requests := &botRequestLog{seen: make(chan string, 8)}
	bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
		requests.add(request.URL.Path)
		return testTelegramResponse(request), nil
	})
	bot.bot.Poller = lifecyclePollerFunc(func(_ *tele.Bot, updates chan tele.Update, stop chan struct{}) {
		updates <- callbackUpdate(callbackData)
		<-stop
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("callback checker did not run")
	}
	seen := map[string]bool{}
	for !seen["/sendMessage"] || !seen["/answerCallbackQuery"] {
		select {
		case path := <-requests.seen:
			if strings.HasSuffix(path, "/sendMessage") {
				seen["/sendMessage"] = true
			}
			if strings.HasSuffix(path, "/answerCallbackQuery") {
				seen["/answerCallbackQuery"] = true
			}
		case <-time.After(time.Second):
			cancel()
			t.Fatalf("callback did not finish reply and acknowledgement: %+v", seen)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Bot.Run error after parent cancellation: %v", err)
	}
}

func TestBotDrainsFullUpdateQueueUntilPollerExits(t *testing.T) {
	requests := &botRequestLog{}
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{AllowedUsers: []int64{42}}), func(request *http.Request) (*http.Response, error) {
		requests.add(request.URL.Path)
		return testTelegramResponse(request), nil
	})
	pollerReturned := make(chan struct{})
	pollerStarted := make(chan struct{})
	bot.bot.Poller = lifecyclePollerFunc(func(_ *tele.Bot, updates chan tele.Update, stop chan struct{}) {
		close(pollerStarted)
		<-stop
		for range cap(updates) + 1 {
			updates <- tele.Update{}
		}
		close(pollerReturned)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- bot.Run(ctx) }()
	select {
	case <-pollerStarted:
	case <-time.After(time.Second):
		t.Fatal("poller did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Bot.Run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Bot.Run did not drain updates blocked behind a full queue")
	}
	select {
	case <-pollerReturned:
	default:
		t.Fatal("Bot.Run returned before poller exit")
	}
	for _, path := range requests.snapshot() {
		if strings.HasSuffix(path, "/sendMessage") {
			t.Fatalf("shutdown update was processed: %s", path)
		}
	}
}

func TestBotNotifierSendWaitsForRuntimeReadiness(t *testing.T) {
	requests := &botRequestLog{}
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
		requests.add(request.URL.Path)
		return testTelegramResponse(request), nil
	})
	if _, err := bot.Send(context.Background(), 42, "early", message.FormatPlainText); !errors.Is(err, ErrBotNotReady) {
		t.Fatalf("pre-run send error = %v, want not ready", err)
	}
	for _, path := range requests.snapshot() {
		if strings.HasSuffix(path, "/sendMessage") {
			t.Fatal("pre-run notifier send reached Telegram")
		}
	}
}

type waitingResponseBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (b *waitingResponseBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*waitingResponseBody) Close() error { return nil }

func TestBotRunCancelsInflightHTTPBodyRead(t *testing.T) {
	bodyRead := make(chan struct{})
	var requestContext context.Context
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/sendMessage") {
			requestContext = request.Context()
			body := &waitingResponseBody{ctx: request.Context(), started: bodyRead}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: request}, nil
		}
		return testTelegramResponse(request), nil
	})
	pollerStarted := make(chan struct{})
	bot.bot.Poller = lifecyclePollerFunc(func(_ *tele.Bot, _ chan tele.Update, stop chan struct{}) {
		close(pollerStarted)
		<-stop
	})
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- bot.Run(ctx) }()
	select {
	case <-pollerStarted:
	case <-time.After(time.Second):
		t.Fatal("poller did not start")
	}
	sendDone := make(chan error, 1)
	go func() {
		_, err := bot.Send(context.Background(), 42, "status", message.FormatPlainText)
		sendDone <- err
	}()
	select {
	case <-bodyRead:
	case <-time.After(time.Second):
		t.Fatal("Telegram request body was not read")
	}
	cancel()
	select {
	case err := <-sendDone:
		if err == nil {
			t.Fatal("in-flight send unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight response body did not observe runtime cancellation")
	}
	if requestContext == nil || requestContext.Err() == nil {
		t.Fatal("runtime cancellation was not attached to request body")
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Bot.Run error after cancellation: %v", err)
	}
}

func TestBotConstructionRequestUsesCallerContext(t *testing.T) {
	started := make(chan struct{})
	var transportContext context.Context
	client := &http.Client{Timeout: time.Second, Transport: lifecycleRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		transportContext = request.Context()
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := NewBot(ctx, "test-token", NewRouter(nil, RouterOptions{}), client, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("NewBot error = %v, want construction context cancellation", err)
	}
	if transportContext == nil || transportContext.Err() == nil {
		t.Fatal("construction request context did not stay attached")
	}
}

func TestBotRunNormalizesStartupCancellationAndIsSingleUse(t *testing.T) {
	requests := &botRequestLog{}
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
		requests.add(request.URL.Path)
		return testTelegramResponse(request), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := bot.Run(ctx); err != nil {
		t.Fatalf("Run with canceled startup context = %v", err)
	}
	for _, path := range requests.snapshot() {
		if strings.HasSuffix(path, "/setMyCommands") {
			t.Fatal("startup cancellation registered remote commands")
		}
	}
	if err := bot.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "once") {
		t.Fatalf("second Run error = %v, want single-use error", err)
	}
}

func TestBotPollerPanicBecomesRedactedFatalError(t *testing.T) {
	bot := newLifecycleBot(t, NewRouter(nil, RouterOptions{}), func(request *http.Request) (*http.Response, error) {
		return testTelegramResponse(request), nil
	})
	bot.bot.Poller = lifecyclePollerFunc(func(*tele.Bot, chan tele.Update, chan struct{}) {
		panic("private poller payload")
	})
	err := bot.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "poller panicked") || strings.Contains(err.Error(), "private poller payload") {
		t.Fatalf("poller panic error = %v", err)
	}
}

func TestBotHandlerPanicBecomesRedactedFatalError(t *testing.T) {
	checker := botStatusCheckFunc(func(context.Context, []string, string) ([]message.Chunk, error) {
		panic("private handler payload")
	})
	router := NewRouter(nil, RouterOptions{AllowedUsers: []int64{42}, Catalog: func() []tfl.Line {
		return []tfl.Line{{ID: "central", DisplayName: "Central"}}
	}, StatusChecker: checker})
	data := callbackDataForCheck(t, router)
	bot := newLifecycleBot(t, router, func(request *http.Request) (*http.Response, error) {
		return testTelegramResponse(request), nil
	})
	bot.bot.Poller = lifecyclePollerFunc(func(_ *tele.Bot, updates chan tele.Update, stop chan struct{}) {
		updates <- callbackUpdate(data)
		<-stop
	})
	err := bot.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "handler panicked") || strings.Contains(err.Error(), "private handler payload") {
		t.Fatalf("handler panic error = %v", err)
	}
}

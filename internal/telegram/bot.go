package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/monitor"
	tele "gopkg.in/telebot.v4"
)

var ErrBotNotReady = logging.Message("telegram bot is not ready")

const defaultTelegramHTTPTimeout = time.Minute

// Bot is the Telebot boundary. All settings behavior remains in Router so it can
// be tested without contacting Telegram.
type Bot struct {
	logger    *slog.Logger
	bot       *tele.Bot
	router    *Router
	transport *lifecycleTransport
	client    *http.Client

	lifecycleMu sync.Mutex
	runStarted  bool
	runtimeCtx  context.Context
	ready       bool
	handlers    sync.WaitGroup

	handlerFatal chan error
}

func NewBot(ctx context.Context, token string, router *Router, client *http.Client, logger *slog.Logger) (*Bot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "telegram")
	logger.DebugContext(ctx, "Initializing Telegram bot", "operation", "get_me")
	clientCopy := http.Client{Timeout: defaultTelegramHTTPTimeout}
	if client != nil {
		clientCopy = *client
		if clientCopy.Timeout <= 0 {
			clientCopy.Timeout = defaultTelegramHTTPTimeout
		}
	}
	baseTransport := clientCopy.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport := &lifecycleTransport{base: baseTransport, ctx: ctx}
	clientCopy.Transport = transport
	settings := tele.Settings{
		Token:       token,
		Poller:      poller{logger: logger, ctx: ctx},
		Client:      &clientCopy,
		Synchronous: true,
		OnError: func(err error, _ tele.Context) {
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Error("Telegram update handler failed", "operation", "handle_update", logging.Error(err))
		},
	}
	bot, err := tele.NewBot(settings)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("create Telegram bot: %w", safeDeliveryError(ctx, err, false))
	}
	logger.DebugContext(ctx, "Telegram bot initialized", "operation", "get_me")
	return &Bot{logger: logger, bot: bot, router: router, transport: transport, client: &clientCopy}, nil
}

func (b *Bot) registerHandlers() {
	for _, command := range []string{"/start", "/help", CommandRoot} {
		cmd := command
		b.bot.Handle(cmd, func(c tele.Context) error {
			return b.admit(c, func(ctx context.Context) error { return b.handle(ctx, c, cmd) })
		})
	}
	b.bot.Handle(tele.OnCallback, func(c tele.Context) error {
		return b.admit(c, func(ctx context.Context) error { return b.handleCallback(ctx, c) })
	})
	b.bot.Handle(tele.OnText, func(c tele.Context) error {
		return b.admit(c, func(ctx context.Context) error { return b.handleText(ctx, c) })
	})
}

func (b *Bot) admit(c tele.Context, handle func(context.Context) error) error {
	b.lifecycleMu.Lock()
	ctx := b.runtimeCtx
	if !b.ready || ctx == nil || ctx.Err() != nil {
		b.lifecycleMu.Unlock()
		return nil //nolint:nilerr // Updates arriving during shutdown are intentionally ignored.
	}
	b.handlers.Add(1)
	b.lifecycleMu.Unlock()

	go func() {
		defer b.handlers.Done()
		defer func() {
			if recover() != nil {
				b.reportHandlerFatal(botPanic{operation: "handler"})
			}
		}()
		if err := handle(ctx); err != nil && !errors.Is(err, context.Canceled) {
			b.bot.OnError(err, c)
		}
	}()
	return nil
}

func (b *Bot) reportHandlerFatal(err error) {
	select {
	case b.handlerFatal <- err:
	default:
	}
}

func (b *Bot) handle(ctx context.Context, c tele.Context, command string) error {
	return b.dispatch(ctx, c, Update{UserID: userID(c), ChatType: chatType(c), Command: command,
		Text: c.Text(), MessageID: messageID(c), ReplyToMessageID: replyToID(c)})
}

func (b *Bot) handleCallback(ctx context.Context, c tele.Context) error {
	callback := c.Callback()
	if callback == nil {
		return nil
	}
	data := callback.Data
	if !strings.HasPrefix(data, CallbackPrefix) {
		data = CallbackPrefix + data
	}
	err := b.dispatch(ctx, c, Update{UserID: userID(c), ChatType: chatType(c), CallbackData: data, MessageID: messageID(c)})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if respondErr := c.Respond(); err == nil {
		err = safeDeliveryError(ctx, respondErr, false)
	}
	return err
}

func (b *Bot) handleText(ctx context.Context, c tele.Context) error {
	return b.dispatch(ctx, c, Update{UserID: userID(c), ChatType: chatType(c), Text: c.Text(), MessageID: messageID(c), ReplyToMessageID: replyToID(c)})
}

func (b *Bot) dispatch(ctx context.Context, c tele.Context, update Update) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	actions, err := b.router.Handle(ctx, update)
	if err != nil {
		return err
	}
	for _, action := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now()
		b.logger.DebugContext(ctx, "Preparing Telegram reply", "operation", string(action.Kind), "user_id", update.UserID)
		markup := toMarkup(action.Keyboard)
		options := actionOptions(action, markup)
		switch action.Kind {
		case ActionEdit:
			target := tele.StoredMessage{MessageID: strconv.FormatInt(action.MessageID, 10), ChatID: c.Chat().ID}
			if _, err := c.Bot().Edit(target, action.Text, options...); err != nil {
				if errors.Is(err, tele.ErrMessageNotModified) || errors.Is(err, tele.ErrSameMessageContent) {
					b.logger.DebugContext(ctx, "Telegram reply is unchanged", "operation", "edit", "user_id", update.UserID)
					continue
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				b.logger.WarnContext(ctx, "Could not edit Telegram reply; sending a new message", "operation", "edit", logging.Error(safeDeliveryError(ctx, err, true)))
				if sendErr := c.Send(action.Text, options...); sendErr != nil {
					return safeDeliveryError(ctx, sendErr, false)
				}
			}
		case ActionForceReply:
			if markup == nil {
				markup = &tele.ReplyMarkup{}
				options = append(options, markup)
			}
			markup.ForceReply = true
			sent, err := c.Bot().Send(c.Chat(), action.Text, options...)
			if err != nil {
				b.router.CompletePromptSend(update.UserID, action.PromptID, 0)
				return safeDeliveryError(ctx, err, false)
			}
			if sent == nil || sent.ID <= 0 {
				b.router.CompletePromptSend(update.UserID, action.PromptID, 0)
				return logging.Message("telegram prompt send returned no message reference")
			}
			b.router.CompletePromptSend(update.UserID, action.PromptID, int64(sent.ID))
		default:
			if err := c.Send(action.Text, options...); err != nil {
				return safeDeliveryError(ctx, err, false)
			}
		}
		b.logger.DebugContext(ctx, "Telegram reply completed", "operation", string(action.Kind), "duration_ms", time.Since(started).Milliseconds())
	}
	return nil
}

func actionOptions(action Action, markup *tele.ReplyMarkup) []any {
	options := make([]any, 0, 2)
	if markup != nil {
		options = append(options, markup)
	}
	if action.Format == message.FormatHTML {
		options = append(options, tele.ModeHTML)
	}
	return options
}

func toMarkup(keyboard [][]Button) *tele.ReplyMarkup {
	if len(keyboard) == 0 {
		return nil
	}
	markup := &tele.ReplyMarkup{InlineKeyboard: make([][]tele.InlineButton, 0, len(keyboard))}
	for _, row := range keyboard {
		buttons := make([]tele.InlineButton, 0, len(row))
		for _, button := range row {
			data := strings.TrimPrefix(button.Data, CallbackPrefix)
			// The router owns the complete tfl: callback namespace. Leaving Unique
			// empty makes Telebot pass callback_data through its generic handler.
			buttons = append(buttons, tele.InlineButton{Text: button.Text, Data: CallbackPrefix + data})
		}
		markup.InlineKeyboard = append(markup.InlineKeyboard, buttons)
	}
	return markup
}

func userID(c tele.Context) int64 {
	if sender := c.Sender(); sender != nil {
		return sender.ID
	}
	if chat := c.Chat(); chat != nil {
		return chat.ID
	}
	return 0
}

func chatType(c tele.Context) string {
	if chat := c.Chat(); chat != nil {
		return string(chat.Type)
	}
	return ""
}

func messageID(c tele.Context) int64 {
	if message := c.Message(); message != nil {
		return int64(message.ID)
	}
	if callback := c.Callback(); callback != nil && callback.Message != nil {
		return int64(callback.Message.ID)
	}
	return 0
}

func replyToID(c tele.Context) int64 {
	if message := c.Message(); message != nil && message.ReplyTo != nil {
		return int64(message.ReplyTo.ID)
	}
	return 0
}

func (b *Bot) registerCommands() error {
	return b.bot.SetCommands([]tele.Command{
		{Text: "tfl", Description: "Configure TfL alerts"},
		{Text: "help", Description: "Show help and available commands"},
	})
}

func (b *Bot) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	b.lifecycleMu.Lock()
	if b.runStarted {
		b.lifecycleMu.Unlock()
		return errors.New("telegram bot can only be run once")
	}
	b.runStarted = true
	b.lifecycleMu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.transport.BindContext(runCtx)
	if p, ok := b.bot.Poller.(poller); ok {
		p.ctx = runCtx
		b.bot.Poller = p
	}
	b.lifecycleMu.Lock()
	b.runtimeCtx = runCtx
	b.handlerFatal = make(chan error, 1)
	b.lifecycleMu.Unlock()
	if runCtx.Err() != nil {
		b.lifecycleMu.Lock()
		b.runtimeCtx = nil
		b.lifecycleMu.Unlock()
		return nil
	}
	b.logger.DebugContext(ctx, "Starting Telegram update handling", "operation", "run")
	b.registerHandlers()
	if err := b.registerCommands(); err != nil {
		if runCtx.Err() != nil {
			return nil
		}
		b.logger.ErrorContext(ctx, "Could not register Telegram commands", "operation", "register_commands", logging.Error(safeDeliveryError(runCtx, err, false)))
	}
	if runCtx.Err() != nil {
		return nil
	}
	b.lifecycleMu.Lock()
	b.ready = true
	b.lifecycleMu.Unlock()
	b.logger.DebugContext(ctx, "Telegram bot ready to handle updates", "operation", "run")

	stopPoller := make(chan struct{})
	pollerResult := make(chan error, 1)
	go func(result chan<- error) { result <- b.runPoller(stopPoller) }(pollerResult)

	var runErr error
	var pollerErr error
	for {
		select {
		case <-runCtx.Done():
			goto shutdown
		case err := <-b.handlerFatal:
			runErr = err
			cancel()
			goto shutdown
		case pollerErr = <-pollerResult:
			pollerResult = nil
			if pollerErr != nil {
				runErr = pollerErr
			} else if runCtx.Err() == nil {
				runErr = logging.Message("telegram poller stopped unexpectedly")
			}
			goto shutdown
		case update := <-b.bot.Updates:
			if runCtx.Err() == nil {
				b.bot.ProcessUpdate(update)
			}
		}
	}

shutdown:
	b.lifecycleMu.Lock()
	b.ready = false
	b.lifecycleMu.Unlock()
	cancel()
	close(stopPoller)
	for pollerResult != nil {
		select {
		case <-b.bot.Updates:
		case pollerErr = <-pollerResult:
			pollerResult = nil
		}
	}

	if pollerErr != nil && runErr == nil {
		runErr = pollerErr
	}
	b.handlers.Wait()
	if runErr == nil {
		select {
		case runErr = <-b.handlerFatal:
		default:
		}
	}
	b.lifecycleMu.Lock()
	b.runtimeCtx = nil
	b.lifecycleMu.Unlock()
	b.logger.DebugContext(ctx, "Telegram update handling stopped", "operation", "run")
	return runErr
}

func (b *Bot) runPoller(stop chan struct{}) (err error) {
	defer func() {
		if recover() != nil {
			err = botPanic{operation: "poller"}
		}
	}()
	b.bot.Poller.Poll(b.bot, b.bot.Updates, stop)
	return nil
}

// deliveryBot gives each Telebot operation its own request cancellation while
// retaining runtime cancellation in the underlying lifecycle transport.
func (b *Bot) deliveryBot(ctx context.Context) (*tele.Bot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.lifecycleMu.Lock()
	ready, runtimeCtx := b.ready, b.runtimeCtx
	b.lifecycleMu.Unlock()
	if !ready || runtimeCtx == nil {
		return nil, ErrBotNotReady
	}
	if err := runtimeCtx.Err(); err != nil {
		return nil, err
	}
	client := *b.client
	client.Transport = &lifecycleTransport{base: b.transport, ctx: ctx}
	return tele.NewBot(tele.Settings{Token: b.bot.Token, URL: b.bot.URL, Client: &client, Offline: true})
}

func (b *Bot) Send(ctx context.Context, userID int64, body string, format message.Format) (int, error) {
	bot, err := b.deliveryBot(ctx)
	if err != nil {
		return 0, err
	}
	sent, err := bot.Send(&tele.User{ID: userID}, body, deliveryOptions(format)...)
	if err != nil {
		return 0, safeDeliveryError(ctx, err, false)
	}
	if sent == nil || sent.ID <= 0 {
		return 0, logging.Message("telegram send returned no message reference")
	}
	return sent.ID, nil
}

func (b *Bot) Edit(ctx context.Context, userID int64, messageID int, body string, format message.Format) error {
	bot, err := b.deliveryBot(ctx)
	if err != nil {
		return err
	}
	_, err = bot.Edit(tele.StoredMessage{MessageID: strconv.Itoa(messageID), ChatID: userID}, body, deliveryOptions(format)...)
	return safeDeliveryError(ctx, err, true)
}

func deliveryOptions(format message.Format) []any {
	if format == message.FormatHTML {
		return []any{tele.ModeHTML}
	}
	return nil
}

type deliveryFailure struct {
	diagnostic string
	delay      time.Duration
	cause      error
}

func (e deliveryFailure) Error() string             { return e.diagnostic }
func (e deliveryFailure) RetryDelay() time.Duration { return e.delay }
func (e deliveryFailure) Unwrap() error             { return e.cause }

func safeDeliveryError(ctx context.Context, err error, edit bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if edit {
		if errors.Is(err, tele.ErrMessageNotModified) || errors.Is(err, tele.ErrSameMessageContent) {
			return nil
		}
		// The pinned Telebot returns an untyped error for this specific response.
		if err.Error() == "telegram: Bad Request: message to edit not found (400)" {
			return deliveryFailure{diagnostic: err.Error(), cause: errors.Join(err, monitor.ErrEditTargetUnavailable)}
		}
		if errors.Is(err, tele.ErrCantEditMessage) {
			return deliveryFailure{diagnostic: err.Error(), cause: errors.Join(err, monitor.ErrEditTargetUnavailable)}
		}
		var apiErr *tele.Error
		if errors.As(err, &apiErr) && apiErr.Code == 400 && apiErr.Description == "Bad Request: message to edit not found" {
			return deliveryFailure{diagnostic: err.Error(), cause: errors.Join(err, monitor.ErrEditTargetUnavailable)}
		}
	}
	if flood, ok := errors.AsType[tele.FloodError](err); ok {
		return deliveryFailure{diagnostic: err.Error(), delay: time.Duration(flood.RetryAfter) * time.Second, cause: err}
	}
	return deliveryFailure{diagnostic: err.Error(), cause: err}
}

type botPanic struct{ operation string }

func (e botPanic) Error() string { return "telegram " + e.operation + " panicked" }

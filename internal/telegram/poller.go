package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	tele "gopkg.in/telebot.v4"
)

type poller struct {
	logger *slog.Logger
	ctx    context.Context
}

func (p poller) Poll(bot *tele.Bot, updates chan tele.Update, stop chan struct{}) {
	offset := 1
	backoff := time.Second
	failures := 0
	logger := p.logger
	if logger == nil {
		logger = slog.Default().With("component", "telegram")
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		started := time.Now()
		logger.Debug("Polling Telegram for updates", "operation", "get_updates", "attempt", failures+1)
		batch, err := pollUpdates(bot, offset)
		if err != nil {
			select {
			case <-stop:
				return
			default:
			}
			failures++
			delay := backoff
			var flood tele.FloodError
			if errors.As(err, &flood) && flood.RetryAfter > 0 {
				delay = max(delay, time.Duration(flood.RetryAfter)*time.Second)
			}
			if p.ctx == nil || !errors.Is(p.ctx.Err(), context.Canceled) {
				logger.Warn("Waiting to retry Telegram update polling", "operation", "get_updates", "duration_ms", time.Since(started).Milliseconds(), "attempt", failures, "retry_delay_ms", delay.Milliseconds(), logging.Error(safeDeliveryError(context.Background(), err, false)))
			}
			timer := time.NewTimer(delay)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		if failures > 0 {
			logger.Info("Telegram update polling recovered", "operation", "get_updates", "failure_count", failures)
		}
		failures = 0
		logger.Debug("Telegram update poll completed", "operation", "get_updates", "duration_ms", time.Since(started).Milliseconds(), "count", len(batch))
		backoff = time.Second
		for _, update := range batch {
			select {
			case <-stop:
				return
			case updates <- update:
				offset = update.ID + 1
			}
		}
	}
}

func pollUpdates(bot *tele.Bot, offset int) ([]tele.Update, error) {
	data, err := bot.Raw("getUpdates", map[string]string{
		"offset": strconv.Itoa(offset), "timeout": "10", "allowed_updates": "null",
	})
	if err != nil {
		return nil, err
	}
	var response struct {
		Result []tele.Update `json:"result"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode Telegram polling response: %w", err)
	}
	if response.Result == nil {
		return nil, logging.Message("telegram polling response has no update array")
	}
	return response.Result, nil
}

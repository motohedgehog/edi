// Package telegram contains the TfL persona router and the Telebot adapter.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

const (
	PersonaID        = "tfl"
	CommandRoot      = "/tfl"
	CallbackPrefix   = "tfl:"
	PrivateChat      = "private"
	GroupChat        = "group"
	UnauthorizedText = "This bot is private"
	DraftLifetime    = 15 * time.Minute
	ManualRateLimit  = 30 * time.Second
)

type Update struct {
	UserID           int64
	ChatType         string
	Command          string
	Text             string
	MessageID        int64
	ReplyToMessageID int64
	CallbackData     string
}

type ActionKind string

const (
	ActionSend       ActionKind = "send"
	ActionEdit       ActionKind = "edit"
	ActionForceReply ActionKind = "force_reply"
	ActionSeparate   ActionKind = "separate"
)

type Action struct {
	PromptID  uint64
	Kind      ActionKind
	Text      string
	Format    message.Format
	MessageID int64
	Keyboard  [][]Button
}

type Button struct {
	Text string
	Data string
}

type StatusChecker interface {
	Check(context.Context, []string, string) ([]message.Chunk, error)
}

type Clock interface{ Now() time.Time }

type screen string

const (
	screenOverview      screen = "overview"
	screenLines         screen = "lines"
	screenHours         screen = "hours"
	screenConfiguration screen = "configuration"
	screenDelete        screen = "delete"
)

type Router struct {
	logger        *slog.Logger
	store         *store.Store
	allowed       map[int64]struct{}
	defaultZone   string
	catalog       func() []tfl.Line
	clock         Clock
	statusChecker StatusChecker
	mu            sync.Mutex
	drafts        map[int64]*Draft
	prompts       map[int64]Prompt
	manualAt      map[int64]time.Time
	nextSession   uint64
	nextPromptID  uint64
}

type Draft struct {
	ShowOverground bool
	Profile        store.Profile
	BaseProfile    store.Profile
	Catalog        []tfl.Line
	Previous       screen
	Screen         screen
	SessionID      uint64
	ExpiresAt      time.Time
}

type Prompt struct {
	ID              uint64
	ScreenMessageID int64
	ReplyMessageID  int64
	UserID          int64
	PersonaID       string
	SessionID       uint64
	Kind            string
	ExpiresAt       time.Time
}

type RouterOptions struct {
	Logger        *slog.Logger
	AllowedUsers  []int64
	DefaultZone   string
	Catalog       func() []tfl.Line
	Clock         Clock
	StatusChecker StatusChecker
}

func NewRouter(repository *store.Store, options RouterOptions) *Router {
	allowed := make(map[int64]struct{}, len(options.AllowedUsers))
	for _, userID := range options.AllowedUsers {
		allowed[userID] = struct{}{}
	}
	clock := options.Clock
	if clock == nil {
		clock = monitor.RealClock{}
	}
	zone := options.DefaultZone
	if zone == "" {
		zone = monitor.LondonTimezone
	}
	catalog := options.Catalog
	if catalog == nil {
		catalog = func() []tfl.Line { return nil }
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{
		logger:        logger.With("component", "telegram"),
		store:         repository,
		allowed:       allowed,
		defaultZone:   zone,
		catalog:       catalog,
		clock:         clock,
		statusChecker: options.StatusChecker,
		drafts:        make(map[int64]*Draft),
		prompts:       make(map[int64]Prompt),
		manualAt:      make(map[int64]time.Time),
	}
}

func (r *Router) Handle(ctx context.Context, update Update) (actions []Action, err error) {
	started := time.Now()
	operation := updateOperation(update)
	defer func() {
		if errors.Is(err, context.Canceled) {
			return
		}
		outcome := "handled"
		if err != nil {
			outcome = "failed"
		} else if len(actions) == 0 {
			outcome = "ignored"
		}
		r.logger.DebugContext(ctx, "Finished processing Telegram update", "operation", operation, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds(), "count", len(actions))
	}()
	if update.ChatType != PrivateChat {
		r.logger.DebugContext(ctx, "Ignoring Telegram update outside a private chat", "outcome", "non_private_chat")
		return nil, nil
	}
	r.logger.DebugContext(ctx, "Processing Telegram update", "operation", operation, "user_id", update.UserID)
	if _, ok := r.allowed[update.UserID]; !ok {
		r.logger.DebugContext(ctx, "Ignoring Telegram update from an unauthorized user", "outcome", "not_allowed", "user_id", update.UserID)
		return []Action{{Kind: ActionSend, Text: UnauthorizedText}}, nil
	}
	if update.CallbackData != "" {
		return r.handleCallback(ctx, update)
	}
	if update.ReplyToMessageID != 0 || update.Text != "" {
		if actions, handled, err := r.handlePrompt(update); handled || err != nil {
			return actions, err
		}
	}
	command := strings.TrimSpace(update.Command)
	if command == "" {
		fields := strings.Fields(update.Text)
		if len(fields) > 0 {
			command = fields[0]
		}
	}
	switch strings.Split(command, "@")[0] {
	case "/start":
		return []Action{{Kind: ActionSend, Text: "Available commands:\n/start or /help — show help\n/tfl — manage TfL alerts configuration.", Keyboard: openMenuKeyboard()}}, nil
	case "/help":
		return []Action{{Kind: ActionSend, Text: "Use /tfl to manage Tfl alerts configuration.", Keyboard: openMenuKeyboard()}}, nil
	case CommandRoot:
		return r.openMenu(ctx, update.UserID)
	default:
		return nil, nil
	}
}

// CompletePromptSend attaches a sent message, or clears a failed send, only for its current prompt.
func (r *Router) CompletePromptSend(userID int64, promptID uint64, messageID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prompt, ok := r.prompts[userID]
	if !ok || prompt.ID != promptID {
		return
	}
	draft := r.drafts[userID]
	now := r.clock.Now()
	if messageID <= 0 || !now.Before(prompt.ExpiresAt) || draft == nil ||
		draft.SessionID != prompt.SessionID || !now.Before(draft.ExpiresAt) {
		delete(r.prompts, userID)
		return
	}
	prompt.ReplyMessageID = messageID
	r.prompts[userID] = prompt
}

func (r *Router) openMenu(ctx context.Context, userID int64) ([]Action, error) {
	r.mu.Lock()
	now := r.clock.Now()
	if draft, ok := r.drafts[userID]; ok && now.Before(draft.ExpiresAt) {
		draft.ExpiresAt = now.Add(DraftLifetime)
		if prompt, promptOK := r.prompts[userID]; promptOK {
			prompt.ExpiresAt = draft.ExpiresAt
			r.prompts[userID] = prompt
		}
		action := screenAction(draft, 0, "")
		action.Kind = ActionSend
		r.mu.Unlock()
		return []Action{action}, nil
	}
	delete(r.drafts, userID)
	delete(r.prompts, userID)
	r.mu.Unlock()

	profile, found, err := r.loadProfile(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !found {
		profile = defaultProfile(userID, r.defaultZone)
	}
	r.mu.Lock()
	draft := r.newDraft(profile, profile, screenOverview)
	r.drafts[userID] = draft
	action := screenAction(draft, 0, "")
	action.Kind = ActionSend
	r.mu.Unlock()
	return []Action{action}, nil
}

func (r *Router) loadProfile(ctx context.Context, userID int64) (store.Profile, bool, error) {
	if r.store == nil {
		return store.Profile{}, false, nil
	}
	profile, found, err := r.store.GetProfile(ctx, userID, PersonaID)
	if err != nil {
		return store.Profile{}, false, fmt.Errorf("load tfl profile: %w", err)
	}
	return profile, found, nil
}

func defaultProfile(userID int64, timezone string) store.Profile {
	return store.Profile{
		UserID:       userID,
		PersonaID:    PersonaID,
		Enabled:      true,
		ScheduleMode: store.SchedulePeak,
		WindowStart:  store.DefaultPeakStart,
		WindowEnd:    store.DefaultPeakEnd,
		Timezone:     timezone,
		Lines:        []string{},
	}
}

func (r *Router) newDraft(profile, base store.Profile, previous screen) *Draft {
	r.nextSession++
	return &Draft{
		Profile:     cloneProfile(profile),
		BaseProfile: cloneProfile(base),
		Catalog:     append([]tfl.Line(nil), r.catalog()...),
		Previous:    previous,
		Screen:      previous,
		SessionID:   r.nextSession,
		ExpiresAt:   r.clock.Now().Add(DraftLifetime),
	}
}

func (r *Router) handleCallback(ctx context.Context, update Update) ([]Action, error) {
	data := strings.TrimPrefix(update.CallbackData, CallbackPrefix)
	if data == "reload" {
		r.mu.Lock()
		delete(r.drafts, update.UserID)
		delete(r.prompts, update.UserID)
		r.mu.Unlock()
		return r.openMenu(ctx, update.UserID)
	}
	if data == "open" {
		return r.openMenu(ctx, update.UserID)
	}
	sessionID, revision, action, value, ok := parseCallback(data)
	if !ok {
		return []Action{expiredAction(update.MessageID)}, nil
	}

	r.mu.Lock()
	draft := r.drafts[update.UserID]
	if draft == nil || r.clock.Now().After(draft.ExpiresAt) || draft.SessionID != sessionID || draft.Profile.Revision != revision {
		r.mu.Unlock()
		return []Action{expiredAction(update.MessageID)}, nil
	}
	draft.ExpiresAt = r.clock.Now().Add(DraftLifetime)
	currentScreen := draft.Screen
	if action == "line_view" && currentScreen == screenLines {
		if value != "main" && value != "overground" {
			r.mu.Unlock()
			return nil, nil
		}
		draft.ShowOverground = value == "overground"
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "line" && currentScreen == screenLines {
		draft.Profile.Lines = toggleLine(draft.Profile.Lines, value)
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "screen" {
		target, valid := parseScreen(value)
		if !valid {
			r.mu.Unlock()
			return nil, nil
		}
		draft.Previous = currentScreen
		draft.Screen = target
		draft.ShowOverground = false
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "schedule" && currentScreen == screenHours {
		if value == store.SchedulePeak || value == store.ScheduleAllDay || value == store.ScheduleCustom {
			draft.Profile.ScheduleMode = value
			draft.Profile.AllDay = value == store.ScheduleAllDay
		}
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "status" && (value == "pause" || value == "enable") {
		draftCopy := cloneDraft(draft)
		r.mu.Unlock()
		return r.setAlertStatus(ctx, update, draftCopy, value == "enable")
	}
	if action == "prompt" && (value == "start" || value == "end" || value == "timezone") {
		if value == "start" || value == "end" {
			if currentScreen != screenHours || draft.Profile.ScheduleMode != store.ScheduleCustom {
				r.mu.Unlock()
				return nil, nil
			}
		}
		if value == "timezone" && currentScreen != screenConfiguration {
			r.mu.Unlock()
			return nil, nil
		}
		prompt := Prompt{
			ScreenMessageID: update.MessageID,
			UserID:          update.UserID,
			PersonaID:       PersonaID,
			SessionID:       draft.SessionID,
			Kind:            value,
			ExpiresAt:       draft.ExpiresAt,
		}
		result := r.newPromptAction(prompt, draft, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "timezone" && value == "london" && currentScreen == screenConfiguration {
		draft.Profile.Timezone = monitor.LondonTimezone
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "check" && value == "selected" && currentScreen == screenLines {
		copyDraft := cloneDraft(draft)
		r.mu.Unlock()
		return r.checkStatus(ctx, update, copyDraft, copyDraft.Profile.Lines, copyDraft.Profile.Timezone)
	}
	if action == "check" && value == "saved" {
		copyDraft := cloneDraft(draft)
		lines := append([]string(nil), copyDraft.BaseProfile.Lines...)
		timezone := copyDraft.BaseProfile.Timezone
		r.mu.Unlock()
		return r.checkStatus(ctx, update, copyDraft, lines, timezone)
	}
	if action == "save" {
		copyDraft := cloneDraft(draft)
		r.mu.Unlock()
		return r.saveScreen(ctx, update, copyDraft)
	}
	if action == "discard" {
		delete(r.prompts, update.UserID)
		draft.Profile = cloneProfile(draft.BaseProfile)
		draft.Screen = draft.Previous
		draft.ExpiresAt = r.clock.Now().Add(DraftLifetime)
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "delete" && currentScreen == screenConfiguration {
		draft.Previous = currentScreen
		draft.Screen = screenDelete
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "keep" && currentScreen == screenDelete {
		draft.Screen = draft.Previous
		result := screenAction(draft, update.MessageID, "")
		r.mu.Unlock()
		return []Action{result}, nil
	}
	if action == "delete_confirm" && currentScreen == screenDelete {
		copyDraft := cloneDraft(draft)
		r.mu.Unlock()
		return r.deleteConfiguration(ctx, update, copyDraft)
	}
	r.mu.Unlock()
	return nil, nil
}

func (r *Router) handlePrompt(update Update) ([]Action, bool, error) {
	r.mu.Lock()
	prompt, ok := r.prompts[update.UserID]
	if !ok || prompt.UserID != update.UserID || prompt.PersonaID != PersonaID ||
		prompt.ReplyMessageID == 0 || prompt.ReplyMessageID != update.ReplyToMessageID {
		r.mu.Unlock()
		return nil, false, nil
	}
	if r.clock.Now().After(prompt.ExpiresAt) {
		delete(r.prompts, update.UserID)
		r.mu.Unlock()
		return []Action{{Kind: ActionSend, Text: "That prompt expired.", Keyboard: openMenuKeyboard()}}, true, nil
	}
	draft := r.drafts[update.UserID]
	if draft == nil || draft.SessionID != prompt.SessionID || r.clock.Now().After(draft.ExpiresAt) {
		delete(r.prompts, update.UserID)
		r.mu.Unlock()
		return []Action{{Kind: ActionSend, Text: "That menu expired.", Keyboard: openMenuKeyboard()}}, true, nil
	}
	value := strings.TrimSpace(update.Text)
	if prompt.Kind == "start" || prompt.Kind == "end" {
		if _, _, err := monitor.ParseHHMM(value); err != nil {
			draft.ExpiresAt = r.clock.Now().Add(DraftLifetime)
			prompt.ExpiresAt = draft.ExpiresAt
			result := r.newPromptAction(prompt, draft, err.Error())
			r.mu.Unlock()
			return []Action{result}, true, nil
		}
		if prompt.Kind == "start" {
			draft.Profile.WindowStart = value
		} else {
			draft.Profile.WindowEnd = value
		}
	}
	if prompt.Kind == "timezone" {
		if err := validateTimezone(value); err != nil {
			draft.ExpiresAt = r.clock.Now().Add(DraftLifetime)
			prompt.ExpiresAt = draft.ExpiresAt
			result := r.newPromptAction(prompt, draft, err.Error())
			r.mu.Unlock()
			return []Action{result}, true, nil
		}
		draft.Profile.Timezone = value
	}
	delete(r.prompts, update.UserID)
	draft.ExpiresAt = r.clock.Now().Add(DraftLifetime)
	result := screenAction(draft, prompt.ScreenMessageID, "")
	r.mu.Unlock()
	return []Action{result}, true, nil
}

func (r *Router) setAlertStatus(ctx context.Context, update Update, draft *Draft, enabled bool) ([]Action, error) {
	if r.store == nil || draft.Profile.ID == 0 {
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "No saved configuration yet.", Keyboard: screenKeyboard(draft)}}, nil
	}
	r.logger.DebugContext(ctx, "Changing alert status", "operation", "set_enabled", "user_id", update.UserID)
	saved, found, err := r.store.SetProfileEnabled(ctx, update.UserID, PersonaID, enabled)
	if err != nil {
		return nil, fmt.Errorf("set tfl alert status: %w", err)
	}
	if !found {
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "No saved configuration yet.", Keyboard: screenKeyboard(draft)}}, nil
	}
	r.logger.InfoContext(ctx, "Alert status changed", "operation", "set_enabled", "enabled", saved.Enabled)
	r.mu.Lock()
	if current := r.drafts[update.UserID]; current != nil && current.SessionID == draft.SessionID {
		current.Profile.Enabled = saved.Enabled
		current.BaseProfile.Enabled = saved.Enabled
		current.BaseProfile.Revision = saved.Revision
		current.Profile.Revision = saved.Revision
		current.ExpiresAt = r.clock.Now().Add(DraftLifetime)
		draft = cloneDraft(current)
	}
	r.mu.Unlock()
	return []Action{screenAction(draft, update.MessageID, "")}, nil
}

func (r *Router) checkStatus(ctx context.Context, update Update, draft *Draft, lines []string, timezone string) ([]Action, error) {
	started := time.Now()
	r.logger.DebugContext(ctx, "Checking selected line statuses", "operation", "check_status", "user_id", update.UserID, "line_count", len(lines))
	if len(lines) == 0 {
		r.logger.DebugContext(ctx, "Skipping status check because no lines are selected", "outcome", "no_lines", "user_id", update.UserID)
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "No lines selected. No line alerts will be sent.", Keyboard: screenKeyboard(draft)}}, nil
	}
	if r.statusChecker == nil {
		r.logger.WarnContext(ctx, "Manual status checker is unavailable", "operation", "check_status", "outcome", "checker_unavailable")
		warning := monitor.RenderAvailability(false, 0, r.clock.Now(), timezone)[0]
		return []Action{{Kind: ActionSeparate, Text: warning.Body, Format: warning.Format, Keyboard: openMenuKeyboard()}}, nil
	}
	r.mu.Lock()
	now := r.clock.Now()
	last := r.manualAt[update.UserID]
	if !last.IsZero() && now.Sub(last) < ManualRateLimit {
		remaining := ManualRateLimit - now.Sub(last)
		r.mu.Unlock()
		r.logger.DebugContext(ctx, "Manual status check rate limit reached", "operation", "check_status", "user_id", update.UserID, "retry_delay_ms", remaining.Milliseconds())
		return []Action{{Kind: ActionSeparate, Text: fmt.Sprintf("⚠️ Check status is available in %s.", remaining.Round(time.Second)), Format: message.FormatPlainText, Keyboard: screenKeyboard(draft)}}, nil
	}
	r.manualAt[update.UserID] = now
	r.mu.Unlock()
	result, err := r.statusChecker.Check(ctx, lines, timezone)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		r.logger.DebugContext(ctx, "Status check failed; showing availability warning", "operation", "check_status", "duration_ms", time.Since(started).Milliseconds(), logging.Error(err))
		warning := monitor.RenderAvailability(false, 0, r.clock.Now(), timezone)[0]
		return []Action{{Kind: ActionSeparate, Text: warning.Body, Format: warning.Format, Keyboard: screenKeyboard(draft)}}, nil
	}
	r.logger.DebugContext(ctx, "Selected line status check completed", "operation", "check_status", "duration_ms", time.Since(started).Milliseconds(), "line_count", len(lines), "count", len(result))
	actions := make([]Action, 0, len(result))
	for index, chunk := range result {
		action := Action{Kind: ActionSeparate, Text: chunk.Body, Format: chunk.Format}
		if index == len(result)-1 {
			action.Keyboard = screenKeyboard(draft)
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func (r *Router) saveScreen(ctx context.Context, update Update, draft *Draft) ([]Action, error) {
	r.logger.DebugContext(ctx, "Saving alert configuration", "operation", "save", "user_id", update.UserID, "screen", string(draft.Screen))
	if draft.Screen != screenLines && draft.Screen != screenHours && draft.Screen != screenConfiguration {
		return []Action{screenAction(draft, update.MessageID, "")}, nil
	}
	if err := validateDraft(draft.Profile); err != nil {
		r.logger.DebugContext(ctx, "Skipping invalid alert configuration", "operation", "save", "outcome", "invalid_draft")
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "⚠️ Cannot save: " + escapeHTML(err.Error()), Format: message.FormatHTML, Keyboard: screenKeyboard(draft)}}, nil
	}
	if r.store == nil {
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "⚠️ Settings storage is unavailable.", Format: message.FormatPlainText, Keyboard: screenKeyboard(draft)}}, nil
	}
	latest, found, err := r.loadProfile(ctx, update.UserID)
	if err != nil {
		return nil, err
	}
	if draft.BaseProfile.ID != 0 && (!found || latest.Revision != draft.BaseProfile.Revision) {
		return conflictAction(update.MessageID), nil
	}
	if draft.BaseProfile.ID == 0 && found {
		return conflictAction(update.MessageID), nil
	}
	merged := cloneProfile(draft.Profile)
	expectedRevision := int64(0)
	if found {
		merged = cloneProfile(latest)
		expectedRevision = latest.Revision
		switch draft.Screen {
		case screenLines:
			merged.Lines = append([]string(nil), draft.Profile.Lines...)
		case screenHours:
			merged.ScheduleMode = draft.Profile.ScheduleMode
			merged.AllDay = draft.Profile.AllDay
			merged.WindowStart = draft.Profile.WindowStart
			merged.WindowEnd = draft.Profile.WindowEnd
		case screenConfiguration:
			merged.Timezone = draft.Profile.Timezone
		}
	}
	if found && screenUnchanged(latest, merged, draft.Screen) {
		r.logger.DebugContext(ctx, "Alert configuration is unchanged", "operation", "save", "user_id", update.UserID)
		return r.finishScreenSave(update, draft, latest)
	}
	saved, err := r.store.SaveProfile(ctx, merged, expectedRevision)
	if err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			return conflictAction(update.MessageID), nil
		}
		return nil, fmt.Errorf("save tfl profile: %w", err)
	}
	r.logger.InfoContext(ctx, "Alert configuration saved", "operation", "save", "screen", string(draft.Screen), "line_count", len(saved.Lines), "enabled", saved.Enabled)
	return r.finishScreenSave(update, draft, saved)
}

func (r *Router) finishScreenSave(update Update, draft *Draft, saved store.Profile) ([]Action, error) {
	r.mu.Lock()
	current := r.drafts[update.UserID]
	if current != nil && current.SessionID == draft.SessionID {
		delete(r.prompts, update.UserID)
		current.Profile = cloneProfile(saved)
		current.BaseProfile = cloneProfile(saved)
		current.Screen = current.Previous
		current.ExpiresAt = r.clock.Now().Add(DraftLifetime)
		draft = cloneDraft(current)
	}
	r.mu.Unlock()
	return []Action{screenAction(draft, update.MessageID, "✓ Changes saved")}, nil
}

func (r *Router) deleteConfiguration(ctx context.Context, update Update, draft *Draft) ([]Action, error) {
	r.logger.DebugContext(ctx, "Deleting alert configuration", "operation", "delete", "user_id", update.UserID)
	if r.store == nil || draft.Profile.ID == 0 {
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "No saved configuration to delete.", Keyboard: screenKeyboard(draft)}}, nil
	}
	if err := r.store.DeleteProfile(ctx, update.UserID, PersonaID); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.ErrorContext(ctx, "Could not delete alert configuration", "operation", "delete", logging.Error(err))
		}
		return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "Could not delete your configuration. Try again.", Keyboard: screenKeyboard(draft)}}, nil
	}
	r.mu.Lock()
	delete(r.drafts, update.UserID)
	delete(r.prompts, update.UserID)
	r.mu.Unlock()
	r.logger.InfoContext(ctx, "Alert configuration deleted", "operation", "delete")
	return []Action{{Kind: ActionEdit, MessageID: update.MessageID, Text: "Configuration deleted.", Keyboard: openMenuKeyboard()}}, nil
}

func validateDraft(profile store.Profile) error {
	if err := validateTimezone(profile.Timezone); err != nil {
		return err
	}
	switch profile.ScheduleMode {
	case store.SchedulePeak, "":
		return nil
	case store.ScheduleAllDay:
		return nil
	case store.ScheduleCustom:
		return monitor.ValidateWindow(profile.WindowStart, profile.WindowEnd, false)
	default:
		return errors.New("choose a valid alert schedule")
	}
}

func validateTimezone(value string) error {
	if value != "UTC" && !strings.Contains(value, "/") {
		return errors.New("use a valid IANA timezone name, such as Europe/London")
	}
	if _, err := time.LoadLocation(value); err != nil {
		return errors.New("use a valid IANA timezone name, such as Europe/London")
	}
	return nil
}

func toggleLine(lines []string, line string) []string {
	line = strings.ToLower(strings.TrimSpace(line))
	for index, existing := range lines {
		if strings.EqualFold(existing, line) {
			result := append([]string(nil), lines...)
			return append(result[:index], result[index+1:]...)
		}
	}
	return append(append([]string(nil), lines...), line)
}

func renderScreen(draft *Draft) string {
	switch draft.Screen {
	case screenLines:
		return renderLines(draft)
	case screenHours:
		return renderHours(draft)
	case screenConfiguration:
		return renderConfiguration(draft)
	case screenDelete:
		return "<b>Delete configuration</b>\nThis stops line alerts and TfL outage notices and removes your saved preferences and unsaved changes."
	default:
		return renderOverview(draft)
	}
}

func renderOverview(draft *Draft) string {
	profile := draft.Profile
	lines := lineNamesHTML(profile.Lines, draft.Catalog)
	if len(profile.Lines) == 0 {
		lines = "No lines selected. No line alerts will be sent."
	}
	parts := []string{
		"<b>TfL overview</b>",
		fmt.Sprintf("Saved lines (%d): %s", len(profile.Lines), lines),
		"Alert status: " + enabledText(profile.Enabled),
		"Schedule: " + escapeHTML(scheduleSummary(profile)),
		"Timezone: " + escapeHTML(profile.Timezone),
	}
	if missing := unavailableLines(profile.Lines, draft.Catalog); len(missing) > 0 {
		parts = append(parts, "⚠️ Unavailable saved lines: "+lineNamesHTML(missing, draft.Catalog))
	}
	if pending := unsavedChangesText(draft); pending != "" {
		parts = append(parts, pending)
	}
	return strings.Join(parts, "\n")
}

func renderLines(draft *Draft) string {
	selected := len(draft.Profile.Lines)
	title := "Choose lines"
	if draft.ShowOverground {
		title = "Choose Overground lines"
	}
	text := fmt.Sprintf("<b>%s</b>\nSelected: %d\nAlert status: %s", title, selected, enabledText(draft.Profile.Enabled))
	text += "\n\nTap a line to select or deselect it."
	if selected == 0 {
		text += "\nNo line alerts will be sent."
	}
	if missing := unavailableLines(draft.Profile.Lines, draft.Catalog); len(missing) > 0 {
		text += "\n⚠️ Unavailable saved lines: " + lineNamesHTML(missing, draft.Catalog)
	}
	if pending := unsavedChangesText(draft); pending != "" {
		text += "\n" + pending
	}
	return text
}

func renderHours(draft *Draft) string {
	text := fmt.Sprintf("<b>Alert hours</b>\nAlert status: %s\nSchedule: %s", enabledText(draft.Profile.Enabled), escapeHTML(scheduleSummary(draft.Profile)))
	switch draft.Profile.ScheduleMode {
	case store.ScheduleCustom:
		text += fmt.Sprintf("\nStart: %s\nEnd: %s\nTimezone: %s", escapeHTML(draft.Profile.WindowStart), escapeHTML(draft.Profile.WindowEnd), escapeHTML(draft.Profile.Timezone))
	case store.SchedulePeak:
		text += "\nPeak hours use London time and follow daylight-saving changes."
	}
	if pending := unsavedChangesText(draft); pending != "" {
		text += "\n" + pending
	}
	return text
}

func renderConfiguration(draft *Draft) string {
	text := fmt.Sprintf("<b>Configuration</b>\nAlert status: %s\nTimezone: %s\nThe timezone controls Custom hours. Peak hours always use London time.",
		enabledText(draft.Profile.Enabled), escapeHTML(draft.Profile.Timezone))
	if pending := unsavedChangesText(draft); pending != "" {
		text += "\n" + pending
	}
	return text
}

func unsavedChangesText(draft *Draft) string {
	if draft == nil {
		return ""
	}
	if sameLineIDs(draft.Profile.Lines, draft.BaseProfile.Lines) &&
		draft.Profile.ScheduleMode == draft.BaseProfile.ScheduleMode &&
		draft.Profile.WindowStart == draft.BaseProfile.WindowStart &&
		draft.Profile.WindowEnd == draft.BaseProfile.WindowEnd &&
		draft.Profile.Timezone == draft.BaseProfile.Timezone {
		return ""
	}
	return "⚠️ Changes need saving."
}

func sameLineIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, id := range a {
		if !containsLine(b, id) {
			return false
		}
	}
	return true
}

func unavailableLines(ids []string, catalog []tfl.Line) []string {
	var missing []string
	for _, id := range ids {
		if !containsCatalog(catalog, id) {
			missing = append(missing, id)
		}
	}
	return missing
}

func lineNamesHTML(ids []string, catalog []tfl.Line) string {
	if len(ids) == 0 {
		return "none"
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := id
		for _, line := range catalog {
			if strings.EqualFold(line.ID, id) {
				name = line.DisplayName
				break
			}
		}
		names = append(names, "<b>"+escapeHTML(name)+"</b>")
	}
	return strings.Join(names, ", ")
}

func escapeHTML(value string) string {
	return html.EscapeString(strings.ToValidUTF8(value, "�"))
}

func screenKeyboard(draft *Draft) [][]Button {
	callback := func(action, value string) string {
		return fmt.Sprintf("%s%d:%d:%s:%s", CallbackPrefix, draft.SessionID, draft.Profile.Revision, action, value)
	}
	switch draft.Screen {
	case screenLines:
		keyboard := make([][]Button, 0, len(draft.Catalog)+3)
		for _, line := range draft.Catalog {
			if (line.ModeName == "overground") != draft.ShowOverground {
				continue
			}
			selected := containsLine(draft.Profile.Lines, line.ID)
			label := line.DisplayName
			if selected {
				label += " ✓"
			}
			keyboard = append(keyboard, []Button{{Text: label, Data: callback("line", line.ID)}})
		}
		for _, lineID := range draft.Profile.Lines {
			if !draft.ShowOverground && !containsCatalog(draft.Catalog, lineID) {
				keyboard = append(keyboard, []Button{{Text: lineID + " ✓", Data: callback("line", lineID)}})
			}
		}
		if draft.ShowOverground {
			keyboard = append(keyboard, []Button{{Text: "Back", Data: callback("line_view", "main")}})
		} else {
			keyboard = append(keyboard, []Button{{Text: "Overground", Data: callback("line_view", "overground")}})
		}
		keyboard = append(keyboard,
			[]Button{{Text: "🔎 Check selected lines", Data: callback("check", "selected")}},
			[]Button{{Text: "Save", Data: callback("save", "")}, {Text: "Discard", Data: callback("discard", "")}},
		)
		appendStatusButton(&keyboard, callback, draft.Profile.Enabled, draft.Profile.ID != 0)
		return keyboard
	case screenHours:
		keyboard := [][]Button{
			{{Text: modeButton("Peak hours", draft.Profile.ScheduleMode == store.SchedulePeak), Data: callback("schedule", store.SchedulePeak)}},
			{{Text: modeButton("All day", draft.Profile.ScheduleMode == store.ScheduleAllDay), Data: callback("schedule", store.ScheduleAllDay)}},
			{{Text: modeButton("Custom hours", draft.Profile.ScheduleMode == store.ScheduleCustom), Data: callback("schedule", store.ScheduleCustom)}},
		}
		if draft.Profile.ScheduleMode == store.ScheduleCustom {
			keyboard = append(keyboard,
				[]Button{{Text: "Start: " + draft.Profile.WindowStart, Data: callback("prompt", "start")}},
				[]Button{{Text: "End: " + draft.Profile.WindowEnd, Data: callback("prompt", "end")}},
			)
		}
		keyboard = append(keyboard, []Button{{Text: "Save", Data: callback("save", "")}, {Text: "Discard", Data: callback("discard", "")}})
		appendStatusButton(&keyboard, callback, draft.Profile.Enabled, draft.Profile.ID != 0)
		return keyboard
	case screenConfiguration:
		keyboard := [][]Button{
			{{Text: modeButton("London", draft.Profile.Timezone == monitor.LondonTimezone), Data: callback("timezone", "london")}},
			{{Text: "Custom timezone", Data: callback("prompt", "timezone")}},
			{{Text: "Save", Data: callback("save", "")}, {Text: "Discard", Data: callback("discard", "")}},
			{{Text: "Delete configuration", Data: callback("delete", "")}},
		}
		appendStatusButton(&keyboard, callback, draft.Profile.Enabled, draft.Profile.ID != 0)
		return keyboard
	case screenDelete:
		return [][]Button{
			{{Text: "Delete configuration", Data: callback("delete_confirm", "")}},
			{{Text: "Keep configuration", Data: callback("keep", "")}},
		}
	default:
		return overviewKeyboard(callback, draft)
	}
}

func overviewKeyboard(callback func(string, string) string, draft *Draft) [][]Button {
	keyboard := [][]Button{
		{{Text: "🚇 Choose lines", Data: callback("screen", string(screenLines))}},
		{{Text: "🕒 Alert hours", Data: callback("screen", string(screenHours))}},
	}
	if draft.Profile.ID != 0 {
		if draft.Profile.Enabled {
			keyboard = append(keyboard, []Button{{Text: "⏸ Pause alerts", Data: callback("status", "pause")}})
		} else {
			keyboard = append(keyboard, []Button{{Text: "▶️ Enable alerts", Data: callback("status", "enable")}})
		}
	}
	keyboard = append(keyboard,
		[]Button{{Text: "🔎 Check status now", Data: callback("check", "saved")}},
		[]Button{{Text: "⚙️ Configuration", Data: callback("screen", string(screenConfiguration))}},
	)
	return keyboard
}

func appendStatusButton(keyboard *[][]Button, callback func(string, string) string, enabled, saved bool) {
	if !saved {
		return
	}
	if enabled {
		*keyboard = append(*keyboard, []Button{{Text: "⏸ Pause alerts", Data: callback("status", "pause")}})
		return
	}
	*keyboard = append(*keyboard, []Button{{Text: "▶️ Enable alerts", Data: callback("status", "enable")}})
}

func screenAction(draft *Draft, messageID int64, prefix string) Action {
	text := renderScreen(draft)
	if prefix != "" {
		text = prefix + "\n\n" + text
	}
	return Action{Kind: ActionEdit, MessageID: messageID, Text: text, Format: message.FormatHTML, Keyboard: screenKeyboard(draft)}
}

func expiredAction(messageID int64) Action {
	return Action{Kind: ActionEdit, MessageID: messageID, Text: "This menu expired. Open it again to continue.", Format: message.FormatPlainText, Keyboard: openMenuKeyboard()}
}

func openMenuKeyboard() [][]Button {
	return [][]Button{{{Text: "Open menu", Data: CallbackPrefix + "open"}}}
}

func conflictAction(messageID int64) []Action {
	return []Action{{Kind: ActionEdit, MessageID: messageID, Text: "⚠️ This configuration changed elsewhere. Reload the screen before saving.", Format: message.FormatPlainText, Keyboard: [][]Button{{{Text: "Reload screen", Data: CallbackPrefix + "reload"}}}}}
}

func parseCallback(value string) (uint64, int64, string, string, bool) {
	parts := strings.SplitN(value, ":", 4)
	if len(parts) < 3 {
		return 0, 0, "", "", false
	}
	sessionID, sessionErr := strconv.ParseUint(parts[0], 10, 64)
	revision, revisionErr := strconv.ParseInt(parts[1], 10, 64)
	if sessionErr != nil || revisionErr != nil || parts[2] == "" {
		return 0, 0, "", "", false
	}
	action := parts[2]
	callbackValue := ""
	if len(parts) == 4 {
		callbackValue = parts[3]
	}
	return sessionID, revision, action, callbackValue, true
}

func parseScreen(value string) (screen, bool) {
	switch screen(value) {
	case screenLines, screenHours, screenConfiguration:
		return screen(value), true
	default:
		return "", false
	}
}

func cloneDraft(draft *Draft) *Draft {
	copyDraft := *draft
	copyDraft.Profile = cloneProfile(draft.Profile)
	copyDraft.BaseProfile = cloneProfile(draft.BaseProfile)
	copyDraft.Catalog = append([]tfl.Line(nil), draft.Catalog...)
	return &copyDraft
}

func cloneProfile(profile store.Profile) store.Profile {
	profile.Lines = append([]string(nil), profile.Lines...)
	return profile
}

func screenUnchanged(saved, desired store.Profile, current screen) bool {
	switch current {
	case screenLines:
		return sameLines(saved.Lines, desired.Lines)
	case screenHours:
		return saved.ScheduleMode == desired.ScheduleMode && saved.AllDay == desired.AllDay &&
			saved.WindowStart == desired.WindowStart && saved.WindowEnd == desired.WindowEnd
	case screenConfiguration:
		return saved.Timezone == desired.Timezone
	default:
		return true
	}
}

func sameLines(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !strings.EqualFold(left[index], right[index]) {
			return false
		}
	}
	return true
}

func containsLine(ids []string, want string) bool {
	for _, id := range ids {
		if strings.EqualFold(id, want) {
			return true
		}
	}
	return false
}

func containsCatalog(catalog []tfl.Line, want string) bool {
	for _, line := range catalog {
		if strings.EqualFold(line.ID, want) {
			return true
		}
	}
	return false
}

func scheduleSummary(profile store.Profile) string {
	switch profile.ScheduleMode {
	case store.SchedulePeak:
		return "Peak hours"
	case store.ScheduleAllDay:
		return "All day"
	case store.ScheduleCustom:
		return fmt.Sprintf("Custom hours (%s–%s)", profile.WindowStart, profile.WindowEnd)
	default:
		return "Peak hours"
	}
}

func modeButton(label string, selected bool) string {
	if selected {
		return label + " ✓"
	}
	return label
}

func enabledText(value bool) string {
	if value {
		return "enabled"
	}
	return "paused"
}

// newPromptAction requires r.mu so replacement and send identity change together.
func (r *Router) newPromptAction(prompt Prompt, draft *Draft, explanation string) Action {
	r.nextPromptID++
	prompt.ID = r.nextPromptID
	prompt.ReplyMessageID = 0
	r.prompts[prompt.UserID] = prompt
	return Action{Kind: ActionForceReply, PromptID: prompt.ID, MessageID: prompt.ScreenMessageID,
		Text: promptText(prompt.Kind, draft.Profile, explanation), Keyboard: screenKeyboard(draft)}
}

func promptText(kind string, profile store.Profile, explanation string) string {
	var text string
	switch kind {
	case "start":
		text = fmt.Sprintf("Current start: %s\nTimezone: %s\nReply with the start time in HH:MM format, for example 06:30.", profile.WindowStart, profile.Timezone)
	case "end":
		text = fmt.Sprintf("Current end: %s\nTimezone: %s\nReply with the end time in HH:MM format, for example 19:00.", profile.WindowEnd, profile.Timezone)
	default:
		text = fmt.Sprintf("Current timezone: %s\nReply with a valid IANA timezone name, for example Europe/London.", profile.Timezone)
	}
	if explanation != "" {
		return "That value is invalid: " + explanation + "\n\n" + text
	}
	return text
}

// updateOperation only returns fixed labels; commands and callback data are untrusted.
func updateOperation(update Update) string {
	if update.CallbackData != "" {
		data := strings.TrimPrefix(update.CallbackData, CallbackPrefix)
		if data == "open" || data == "reload" {
			return data
		}
		_, _, action, _, ok := parseCallback(data)
		if ok {
			switch action {
			case "line_view", "line", "screen", "schedule", "status", "prompt", "timezone", "check", "save", "discard", "delete", "keep", "delete_confirm":
				return action
			}
		}
		return "callback"
	}
	command := strings.TrimSpace(update.Command)
	if command == "" {
		fields := strings.Fields(update.Text)
		if len(fields) > 0 {
			command = fields[0]
		}
	}
	switch strings.Split(command, "@")[0] {
	case "/start":
		return "start"
	case "/help":
		return "help"
	case CommandRoot:
		return "menu"
	}
	return "text"
}

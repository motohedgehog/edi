package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

type automaticLines struct {
	logger   *slog.Logger
	repo     ProfileRepository
	notifier Notifier
	clock    Clock
}

type lineChange struct {
	alert     AlertLine
	candidate store.CandidateState
}

func (a *automaticLines) prepare(ctx context.Context, profile store.Profile, selected map[string]tfl.LineStatus, observedAt, now time.Time) (int, error) {
	states, err := a.repo.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		return 0, fmt.Errorf("load monitor states: %w", err)
	}
	mode := profileScheduleMode(profile)
	inWindow, period, expiresAt := SchedulePeriod(observedAt, mode, profile.Timezone, profile.WindowStart, profile.WindowEnd)
	window := fmt.Sprintf("%d:%s", profile.Generation, period)
	changes, baseline := compareLines(profile, states, selected, observedAt, inWindow, window)
	a.logger.DebugContext(ctx, "Compared selected line statuses", "user_id", profile.UserID, "selected_count", len(selected), "changed_count", len(changes), "baseline_count", len(baseline), "in_window", inWindow)
	if !inWindow {
		a.logger.DebugContext(ctx, "Suppressing alerts outside the notification schedule", "outcome", "outside_schedule", "user_id", profile.UserID)
	} else if len(changes) == 0 {
		a.logger.DebugContext(ctx, "No line status changes to notify", "user_id", profile.UserID)
	}
	if len(baseline) > 0 {
		if err := a.repo.SaveMonitorStates(ctx, baseline); err != nil {
			return 0, fmt.Errorf("save monitoring baseline: %w", err)
		}
	}
	alertTimezone := profile.Timezone
	if mode == SchedulePeak {
		alertTimezone = LondonTimezone
	}
	created := 0
	for _, lineID := range profile.Lines {
		var pending *store.OutboxChunk
		lineWindow := window
		if !inWindow && states[lineID].Generation == profile.Generation {
			lineWindow = states[lineID].WindowInstance
		}
		if change, ok := changes[lineID]; ok {
			body, err := RenderAutomaticAlert(change.alert, observedAt, alertTimezone)
			if err != nil {
				return created, err
			}
			lineExpiry := expiresAt
			if change.alert.Kind == "recovery" {
				lineExpiry = time.Time{}
			}
			chunks := chunksForBody([]message.Chunk{body}, observedAt, profile, []string{lineID}, lineExpiry)
			pending = &chunks[0]
			pending.Candidates = []store.CandidateState{change.candidate}
			lineWindow = change.candidate.WindowInstance
			pending.Operation = "send"
			old := states[lineID]
			if canEdit(old, change.candidate, now) {
				pending.Operation = "edit"
				pending.MessageID = old.MessageID
			}
			created++
		}
		if err := a.repo.ReconcileLine(ctx, pending, profile.ID, profile.Generation, lineWindow, lineID); err != nil {
			return created, fmt.Errorf("reconcile line alert: %w", err)
		}
	}
	return created, nil
}

type lineDeliveryResult struct {
	delivered   bool
	deliveryErr error
}

func (a *automaticLines) deliver(ctx context.Context, profile store.Profile, chunk store.OutboxChunk, now time.Time) (lineDeliveryResult, error) {
	states, err := a.repo.GetMonitorStates(ctx, profile.ID)
	if err != nil {
		return lineDeliveryResult{}, err
	}
	candidate := chunk.Candidates[0]
	old := states[candidate.LineID]
	if old.Generation == candidate.Generation && old.WindowInstance == candidate.WindowInstance && old.Fingerprint == candidate.Fingerprint {
		a.logger.DebugContext(ctx, "Discarding alert because its status is unchanged", "outbox_id", chunk.ID, "outcome", "unchanged")
		return lineDeliveryResult{}, a.repo.DiscardChunk(ctx, chunk)
	}
	var messageID int
	var deliveryErr error
	var sentAt time.Time
	if canEdit(old, candidate, now) {
		chunk.Operation = "edit"
		messageID = old.MessageID
		chunk.MessageID = old.MessageID
		sentAt = old.SentAt
		a.logger.DebugContext(ctx, "Editing alert message", "operation", "edit", "outbox_id", chunk.ID, "user_id", profile.UserID, "attempt", chunk.Attempts+1)
		deliveryErr = a.notifier.Edit(ctx, profile.UserID, messageID, chunk.Body, chunk.Format)
		if errors.Is(deliveryErr, ErrEditTargetUnavailable) {
			a.logger.WarnContext(ctx, "Alert message unavailable for editing", "operation", "edit", "outbox_id", chunk.ID, "outcome", "edit_target_unavailable")
			if err := a.repo.RetireEditTarget(ctx, chunk); err != nil {
				return lineDeliveryResult{}, err
			}
			current, found, err := a.repo.GetProfileByID(ctx, chunk.ProfileID)
			if err != nil {
				return lineDeliveryResult{}, err
			}
			now = a.clock.Now()
			if !found || !a.permitted(current, chunk, now) || (!chunk.ExpiresAt.IsZero() && !now.Before(chunk.ExpiresAt)) {
				a.logger.DebugContext(ctx, "Discarding replacement alert because it is no longer permitted", "outbox_id", chunk.ID, "outcome", "replacement_no_longer_permitted")
				return lineDeliveryResult{}, a.repo.DiscardChunk(ctx, chunk)
			}
			chunk.Operation = "send"
			a.logger.DebugContext(ctx, "Sending alert message", "operation", "send", "outbox_id", chunk.ID, "user_id", profile.UserID, "attempt", chunk.Attempts+1)
			messageID, deliveryErr = a.notifier.Send(ctx, profile.UserID, chunk.Body, chunk.Format)
			sentAt = a.clock.Now()
		}
	} else {
		chunk.Operation = "send"
		a.logger.DebugContext(ctx, "Sending alert message", "operation", "send", "outbox_id", chunk.ID, "user_id", profile.UserID, "attempt", chunk.Attempts+1)
		messageID, deliveryErr = a.notifier.Send(ctx, profile.UserID, chunk.Body, chunk.Format)
		sentAt = a.clock.Now()
	}
	if deliveryErr != nil {
		//nolint:nilerr // Transport failures use the coordinator's retry path.
		return lineDeliveryResult{deliveryErr: deliveryErr}, nil
	}
	a.logger.DebugContext(ctx, "Telegram accepted alert delivery", "operation", chunk.Operation, "outbox_id", chunk.ID)
	if err := a.repo.CompleteLine(ctx, chunk, messageID, sentAt); err != nil {
		return lineDeliveryResult{}, deliveryPersistenceFailure{outboxID: chunk.ID, err: err}
	}
	a.logger.InfoContext(ctx, "Alert delivery completed", "operation", chunk.Operation, "outbox_id", chunk.ID, "attempt", chunk.Attempts+1)
	return lineDeliveryResult{delivered: true}, nil
}

func compareLines(profile store.Profile, previous map[string]store.MonitorState, selected map[string]tfl.LineStatus, observed time.Time, inWindow bool, windowInstance string) (map[string]lineChange, []store.MonitorState) {
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	changes := make(map[string]lineChange, len(selected))
	var baseline []store.MonitorState
	for _, id := range ids {
		current := selected[id]
		candidate := store.CandidateState{LineID: id, Generation: profile.Generation, WindowInstance: windowInstance, Fingerprint: current.Fingerprint, SeverityKey: tfl.SeverityKey(current),
			Good: current.Good, Unknown: current.Unknown, Description: firstDescription(current), Reason: firstReason(current), ObservedAt: observed.UnixMilli()}
		old, exists := previous[id]
		// Close delivered non-good alerts before a new period can replace their baseline.
		recovered := exists && old.Generation == profile.Generation && !old.Good &&
			!old.SentAt.IsZero() && current.Good && !current.Unknown
		if recovered {
			candidate.WindowInstance = old.WindowInstance
			changes[id] = lineChange{alert: AlertLine{LineID: id, DisplayName: current.DisplayName, Kind: "recovery", Statuses: current.Statuses}, candidate: candidate}
			continue
		}
		if !inWindow {
			continue
		}
		if !exists || old.Generation != profile.Generation || old.WindowInstance != windowInstance {
			if current.Good && !current.Unknown {
				baseline = append(baseline, stateFrom(profile, current, observed, windowInstance))
				continue
			}
			changes[id] = lineChange{alert: AlertLine{LineID: id, DisplayName: current.DisplayName, Kind: kindFor(current, nil, false), Statuses: current.Statuses}, candidate: candidate}
			continue
		}
		if old.Fingerprint == current.Fingerprint {
			continue
		}
		kind := kindFor(current, &old, true)
		changes[id] = lineChange{alert: AlertLine{LineID: id, DisplayName: current.DisplayName, Kind: kind, Statuses: current.Statuses}, candidate: candidate}
	}
	return changes, baseline
}

func stateFrom(profile store.Profile, line tfl.LineStatus, observed time.Time, windowInstance string) store.MonitorState {
	return store.MonitorState{ProfileID: profile.ID, LineID: line.LineID, Generation: profile.Generation, WindowInstance: windowInstance, Fingerprint: line.Fingerprint, SeverityKey: tfl.SeverityKey(line),
		Good: line.Good, Unknown: line.Unknown, Description: firstDescription(line), Reason: firstReason(line), ObservedAt: observed}
}

func kindFor(current tfl.LineStatus, old *store.MonitorState, changed bool) string {
	if current.Unknown {
		return "unknown"
	}
	if !changed || old == nil {
		return "disruption"
	}
	if current.Good {
		return "recovery"
	}
	if old.Good {
		return "disruption"
	}
	return "change"
}

func firstDescription(line tfl.LineStatus) string {
	if len(line.Statuses) == 0 {
		return "Unknown"
	}
	return line.Statuses[0].Description
}

func firstReason(line tfl.LineStatus) string {
	if len(line.Statuses) == 0 {
		return ""
	}
	return line.Statuses[0].Reason
}

func (a *automaticLines) permitted(profile store.Profile, chunk store.OutboxChunk, now time.Time) bool {
	if !profile.Enabled || profile.Generation != chunk.Generation {
		return false
	}
	for _, lineID := range chunk.LineIDs {
		if !slices.Contains(profile.Lines, lineID) {
			return false
		}
	}
	if len(chunk.Candidates) == 1 && chunk.Candidates[0].Good && !chunk.Candidates[0].Unknown {
		return true
	}
	return ScheduleActive(now, profileScheduleMode(profile), profile.Timezone, profile.WindowStart, profile.WindowEnd)
}

func canEdit(old store.MonitorState, candidate store.CandidateState, now time.Time) bool {
	return old.MessageID > 0 && !old.SentAt.IsZero() && !now.Before(old.SentAt) && now.Sub(old.SentAt) < DescriptionEditPeriod &&
		old.Generation == candidate.Generation && old.WindowInstance == candidate.WindowInstance && old.SeverityKey != "" && old.SeverityKey == candidate.SeverityKey
}

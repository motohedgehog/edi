package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

type StatusSource interface {
	Status(context.Context) (tfl.Snapshot, error)
}

const DescriptionEditPeriod = 30 * time.Minute

var ErrEditTargetUnavailable = errors.New("message edit target unavailable")

// RetryDelay is implemented by safe delivery failures with a server retry delay.
type RetryDelay interface{ RetryDelay() time.Duration }

type Notifier interface {
	Send(context.Context, int64, string, message.Format) (int, error)
	Edit(context.Context, int64, int, string, message.Format) error
}

type ProfileRepository interface {
	ReconcileLine(context.Context, *store.OutboxChunk, int64, int64, string, string) error
	CompleteLine(context.Context, store.OutboxChunk, int, time.Time) error
	RetireEditTarget(context.Context, store.OutboxChunk) error
	ListProfiles(context.Context, string) ([]store.Profile, error)
	GetProfileByID(context.Context, int64) (store.Profile, bool, error)
	GetMonitorStates(context.Context, int64) (map[string]store.MonitorState, error)
	SaveMonitorStates(context.Context, []store.MonitorState) error
	QueueAvailability(context.Context, int64, int64, bool, []store.OutboxChunk) (bool, error)
	PendingChunks(context.Context, time.Time, int) ([]store.OutboxChunk, error)
	DiscardChunk(context.Context, store.OutboxChunk) error
	RescheduleChunk(context.Context, int64, int, time.Time) error
}

type Service struct {
	logger           *slog.Logger
	repo             ProfileRepository
	status           StatusSource
	notifier         Notifier
	lines            automaticLines
	catalog          func() []tfl.Line
	allowed          map[int64]struct{}
	failureThreshold int
	clock            Clock
	mu               sync.Mutex
	failures         int
	cycleRunning     bool
}

type ServiceOptions struct {
	Logger           *slog.Logger
	Catalog          func() []tfl.Line
	AllowedUsers     []int64
	FailureThreshold int
	Clock            Clock
}

func NewService(repo ProfileRepository, status StatusSource, notifier Notifier, opts ServiceOptions) *Service {
	allowed := make(map[int64]struct{}, len(opts.AllowedUsers))
	for _, userID := range opts.AllowedUsers {
		allowed[userID] = struct{}{}
	}
	threshold := opts.FailureThreshold
	if threshold <= 0 {
		threshold = 3
	}
	clock := opts.Clock
	if clock == nil {
		clock = RealClock{}
	}
	catalog := opts.Catalog
	if catalog == nil {
		catalog = func() []tfl.Line { return nil }
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "monitor")
	return &Service{logger: logger, repo: repo, status: status, notifier: notifier, catalog: catalog,
		allowed: allowed, failureThreshold: threshold, clock: clock,
		lines: automaticLines{repo: repo, notifier: notifier, clock: clock, logger: logger}}
}

type CycleResult struct {
	ProfilesChecked int
	AlertsCreated   int
	Delivered       int
	Failed          bool
}

// Cycle performs one non-overlapping status request and fans it out to enabled profiles.
func (s *Service) Cycle(ctx context.Context) (result CycleResult, err error) {
	started := time.Now()
	s.mu.Lock()
	if s.cycleRunning {
		s.mu.Unlock()
		s.logger.DebugContext(ctx, "Skipping monitor cycle because another cycle is running", "outcome", "already_running")
		return CycleResult{}, nil
	}
	s.cycleRunning = true
	s.mu.Unlock()
	s.logger.DebugContext(ctx, "Monitor cycle started", "operation", "cycle")
	defer func() {
		s.mu.Lock()
		s.cycleRunning = false
		s.mu.Unlock()
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		logger := s.logger.With("duration_ms", time.Since(started).Milliseconds(), "profiles_checked", result.ProfilesChecked,
			"alerts_created", result.AlertsCreated, "delivered", result.Delivered, "upstream_failed", result.Failed)
		if err != nil {
			if persistence, ok := errors.AsType[deliveryPersistenceFailure](err); ok {
				logger.ErrorContext(ctx, "Accepted delivery could not be persisted", "outbox_id", persistence.outboxID, "remote_accepted", true, logging.Error(err))
			} else {
				logger.ErrorContext(ctx, "Monitor cycle failed", logging.Error(err))
			}
			return
		}
		logger.InfoContext(ctx, "Monitor cycle completed")
	}()

	now := s.clock.Now()
	catalog := s.catalog()
	profiles, err := s.repo.ListProfiles(ctx, store.PersonaTFL)
	if err != nil {
		return CycleResult{}, fmt.Errorf("list monitoring profiles: %w", err)
	}
	enabled := make([]store.Profile, 0, len(profiles))
	recovery := make([]store.Profile, 0, len(profiles))
	configured := make([]store.Profile, 0, len(profiles))
	for _, profile := range profiles {
		if _, ok := s.allowed[profile.UserID]; !ok {
			continue
		}
		configured = append(configured, profile)
		if profile.Enabled && len(profile.Lines) > 0 {
			enabled = append(enabled, profile)
		}
		if profile.OutageNotified && profile.OutageGeneration == profile.Generation {
			recovery = append(recovery, profile)
		}
	}
	if len(configured) == 0 {
		s.mu.Lock()
		s.failures = 0
		s.mu.Unlock()
		return CycleResult{}, nil
	}

	snapshot, statusErr := s.status.Status(ctx)
	if statusErr != nil {
		s.mu.Lock()
		s.failures++
		failures := s.failures
		s.mu.Unlock()
		result = CycleResult{ProfilesChecked: len(enabled), Failed: true}
		if failures >= s.failureThreshold {
			for _, profile := range configured {
				if profile.OutageNotified && profile.OutageGeneration == profile.Generation {
					continue
				}
				body := RenderAvailability(false, failures, now, profile.Timezone)
				chunks := chunksForBody(body, now, profile, nil, time.Time{})
				applied, err := s.repo.QueueAvailability(ctx, profile.ID, profile.Generation, true, chunks)
				if err != nil {
					return result, fmt.Errorf("queue TfL outage alert: %w", err)
				}
				if applied {
					result.AlertsCreated++
				}
			}
		}
		delivered, deliverErr := s.deliverPending(ctx, s.clock.Now())
		if deliverErr != nil {
			return result, deliverErr
		}
		result.Delivered = delivered
		return result, nil
	}
	s.mu.Lock()
	previousFailures := s.failures
	s.failures = 0
	s.mu.Unlock()
	if previousFailures > 0 {
		s.logger.InfoContext(ctx, "TfL status updates resumed", "failure_count", previousFailures)
	}

	result = CycleResult{ProfilesChecked: len(enabled)}
	for _, profile := range enabled {
		selected := tfl.ForSelection(snapshot, profile.Lines, catalog)
		created, err := s.lines.prepare(ctx, profile, selected, snapshot.ObservedAt, now)
		result.AlertsCreated += created
		if err != nil {
			return result, err
		}
	}
	for _, profile := range recovery {
		body := RenderAvailability(true, 0, snapshot.ObservedAt, profile.Timezone)
		chunks := chunksForBody(body, snapshot.ObservedAt, profile, nil, time.Time{})
		applied, err := s.repo.QueueAvailability(ctx, profile.ID, profile.Generation, false, chunks)
		if err != nil {
			return result, fmt.Errorf("queue TfL recovery alert: %w", err)
		}
		if applied {
			result.AlertsCreated++
		}
	}
	delivered, err := s.deliverPending(ctx, s.clock.Now())
	if err != nil {
		return result, err
	}
	result.Delivered = delivered
	return result, nil
}

func chunksForBody(body []message.Chunk, observed time.Time, profile store.Profile, lineIDs []string, expiresAt time.Time) []store.OutboxChunk {
	chunks := make([]store.OutboxChunk, 0, len(body))
	for index, rendered := range body {
		chunks = append(chunks, store.OutboxChunk{ProfileID: profile.ID, Revision: profile.Revision, Generation: profile.Generation,
			LineIDs: append([]string(nil), lineIDs...), Body: rendered.Body, Format: rendered.Format, Order: index + 1, Total: len(body),
			NextAttempt: observed, ExpiresAt: expiresAt})
	}
	return chunks
}

func (s *Service) deliverPending(ctx context.Context, now time.Time) (int, error) {
	if s.notifier == nil {
		return 0, nil
	}
	chunks, err := s.repo.PendingChunks(ctx, now, 100)
	if err != nil {
		return 0, fmt.Errorf("load pending alerts: %w", err)
	}
	delivered := 0
	for _, chunk := range chunks {
		now = s.clock.Now()
		profile, ok, profileErr := s.repo.GetProfileByID(ctx, chunk.ProfileID)
		if profileErr != nil {
			return delivered, profileErr
		}
		if !ok {
			if err := s.repo.DiscardChunk(ctx, chunk); err != nil {
				return delivered, err
			}
			s.logger.DebugContext(ctx, "Discarded alert because its profile no longer exists", "outbox_id", chunk.ID, "outcome", "profile_missing")
			continue
		}
		if chunk.EventType == "status" && !s.lines.permitted(profile, chunk, now) {
			if err := s.repo.DiscardChunk(ctx, chunk); err != nil {
				return delivered, err
			}
			s.logger.DebugContext(ctx, "Discarded alert because it is no longer permitted", "outbox_id", chunk.ID, "outcome", "no_longer_permitted")
			continue
		}
		if !chunk.ExpiresAt.IsZero() && !now.Before(chunk.ExpiresAt) {
			if err := s.repo.DiscardChunk(ctx, chunk); err != nil {
				return delivered, err
			}
			s.logger.DebugContext(ctx, "Discarded expired alert", "outbox_id", chunk.ID, "outcome", "expired")
			continue
		}
		if _, allowed := s.allowed[profile.UserID]; !allowed {
			s.logger.DebugContext(ctx, "Skipping alert delivery for an unauthorized user", "outbox_id", chunk.ID, "outcome", "not_allowed", "user_id", profile.UserID)
			continue
		}
		var deliveryErr error
		if chunk.EventType == "status" && len(chunk.Candidates) == 1 {
			result, err := s.lines.deliver(ctx, profile, chunk, now)
			if err != nil {
				return delivered, err
			}
			if result.delivered {
				delivered++
			}
			deliveryErr = result.deliveryErr
		} else {
			s.logger.DebugContext(ctx, "Sending alert message", "operation", "send", "outbox_id", chunk.ID, "user_id", profile.UserID, "attempt", chunk.Attempts+1)
			_, deliveryErr = s.notifier.Send(ctx, profile.UserID, chunk.Body, chunk.Format)
			if deliveryErr == nil {
				s.logger.DebugContext(ctx, "Telegram accepted alert delivery", "operation", "send", "outbox_id", chunk.ID)
				if err := s.repo.DiscardChunk(ctx, chunk); err != nil {
					return delivered, deliveryPersistenceFailure{outboxID: chunk.ID, err: err}
				}
				s.logger.InfoContext(ctx, "Alert delivery completed", "operation", "send", "outbox_id", chunk.ID, "attempt", chunk.Attempts+1)
				delivered++
			}
		}
		if deliveryErr != nil {
			if ctx.Err() != nil {
				return delivered, ctx.Err()
			}
			attempts := chunk.Attempts + 1
			delay := time.Duration(1<<min(attempts, 5)) * time.Second
			var retry RetryDelay
			if errors.As(deliveryErr, &retry) {
				delay = max(delay, retry.RetryDelay())
			}
			if err := s.repo.RescheduleChunk(ctx, chunk.ID, attempts, s.clock.Now().Add(delay)); err != nil {
				return delivered, errors.Join(deliveryErr, err)
			}
			s.logger.WarnContext(ctx, "Alert delivery failed; retry scheduled", "operation", "deliver", "outbox_id", chunk.ID, "attempt", attempts, "retry_delay_ms", delay.Milliseconds(), logging.Error(deliveryErr))
			continue
		}
	}
	return delivered, nil
}

func profileScheduleMode(profile store.Profile) string {
	if profile.ScheduleMode != "" {
		return profile.ScheduleMode
	}
	if profile.AllDay {
		return ScheduleAllDay
	}
	if profile.WindowStart == "" && profile.WindowEnd == "" {
		return SchedulePeak
	}
	return ScheduleCustom
}

func locationFor(timezone string) *time.Location {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.UTC
	}
	return location
}

// deliveryPersistenceFailure distinguishes remote acceptance from a durable completion.
type deliveryPersistenceFailure struct {
	outboxID int64
	err      error
}

func (e deliveryPersistenceFailure) Error() string { return "persist accepted delivery failed" }
func (e deliveryPersistenceFailure) Unwrap() error { return e.err }

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/motohedgehog/edi/internal/message"
)

func optionalTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().UnixMilli()
}

// ReconcileLine replaces pending content without restarting a temporary failure's retry delay.
// A nil chunk cancels work for the current monitoring period.
func (s *Store) ReconcileLine(ctx context.Context, chunk *OutboxChunk, profileID, generation int64, window, lineID string) error {
	return s.write(ctx, func(conn *sql.Conn) error {
		var revision int64
		err := conn.QueryRowContext(ctx, `SELECT revision FROM profiles WHERE id = ? AND generation = ? AND enabled = 1`, profileID, generation).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("load profile for line reconciliation: %w", err)
		}
		lines, err := json.Marshal([]string{lineID})
		if err != nil {
			return fmt.Errorf("encode pending line: %w", err)
		}
		if chunk == nil {
			if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE profile_id=? AND generation=? AND event_type='status' AND line_ids=? AND window_instance=?`, profileID, generation, string(lines), window); err != nil {
				return fmt.Errorf("cancel pending line: %w", err)
			}
			return nil
		}
		if len(chunk.Candidates) != 1 || chunk.Candidates[0].LineID != lineID || chunk.Candidates[0].Generation != generation || chunk.Candidates[0].WindowInstance != window {
			return errors.New("reconcile line: mismatched candidate")
		}
		if chunk.Format != message.FormatPlainText && chunk.Format != message.FormatHTML {
			return errors.New("reconcile line: invalid message format")
		}
		operation := chunk.Operation
		if operation == "" {
			operation = "send"
		}
		if operation != "send" && operation != "edit" || operation == "edit" && chunk.MessageID <= 0 {
			return errors.New("reconcile line: invalid operation")
		}
		candidates, err := json.Marshal(chunk.Candidates)
		if err != nil {
			return fmt.Errorf("encode pending line candidate: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE profile_id=? AND generation=? AND event_type='status' AND line_ids=? AND window_instance<>?`, profileID, generation, string(lines), window); err != nil {
			return fmt.Errorf("retire old monitoring period: %w", err)
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO outbox_chunks
   (profile_id, revision, generation, event_type, line_ids, body, message_format, chunk_order, chunk_total,
    candidate_states, next_attempt_at, created_at, expires_at, operation, message_id, window_instance)
   VALUES (?, ?, ?, 'status', ?, ?, ?, 1, 1, ?, ?, ?, ?, ?, ?, ?)
   ON CONFLICT(profile_id, generation, event_type, line_ids, chunk_order) DO UPDATE SET
    revision=excluded.revision, body=excluded.body, message_format=excluded.message_format,
    candidate_states=excluded.candidate_states, expires_at=excluded.expires_at, operation=excluded.operation,
    message_id=excluded.message_id, version=outbox_chunks.version+1
   WHERE json_extract(outbox_chunks.candidate_states, '$[0].fingerprint') <> json_extract(excluded.candidate_states, '$[0].fingerprint')
    OR outbox_chunks.operation <> excluded.operation OR outbox_chunks.message_id <> excluded.message_id
    OR json_extract(outbox_chunks.candidate_states, '$[0].observed_at') < json_extract(excluded.candidate_states, '$[0].observed_at')`,
			profileID, revision, generation, string(lines), chunk.Body, chunk.Format.String(), string(candidates), chunk.NextAttempt.UTC().UnixMilli(), time.Now().UTC().UnixMilli(), optionalTime(chunk.ExpiresAt), operation, chunk.MessageID, window)
		if err != nil {
			return fmt.Errorf("reconcile pending line: %w", err)
		}
		return nil
	})
}

// CompleteLine atomically records the accepted message and delivered state, provided
// neither the profile generation nor the pending operation changed while Telegram ran.
func (s *Store) CompleteLine(ctx context.Context, chunk OutboxChunk, messageID int, sentAt time.Time) error {
	if len(chunk.Candidates) != 1 || messageID <= 0 || sentAt.IsZero() {
		return errors.New("complete line: delivery receipt is required")
	}
	if len(chunk.LineIDs) != 1 || chunk.Candidates[0].LineID != chunk.LineIDs[0] || chunk.Candidates[0].Generation != chunk.Generation {
		return errors.New("complete line: mismatched candidate")
	}
	return s.write(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `DELETE FROM outbox_chunks WHERE id=? AND version=? AND profile_id=? AND generation=? AND window_instance=? AND event_type='status'
   AND EXISTS (SELECT 1 FROM profiles WHERE id=outbox_chunks.profile_id AND generation=? AND enabled=1)`, chunk.ID, chunk.Version, chunk.ProfileID, chunk.Generation, chunk.Candidates[0].WindowInstance, chunk.Generation)
		if err != nil {
			return fmt.Errorf("remove completed line operation: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check completed line operation: %w", err)
		}
		if count == 0 {
			return nil
		}
		candidate := chunk.Candidates[0]
		_, err = conn.ExecContext(ctx, `INSERT INTO monitor_states
   (profile_id,line_id,generation,window_instance,fingerprint,good,unknown,description,reason,observed_at,severity_key,message_id,sent_at)
   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
   ON CONFLICT(profile_id,line_id) DO UPDATE SET generation=excluded.generation,window_instance=excluded.window_instance,
    fingerprint=excluded.fingerprint,good=excluded.good,unknown=excluded.unknown,description=excluded.description,
    reason=excluded.reason,observed_at=excluded.observed_at,severity_key=excluded.severity_key,message_id=excluded.message_id,sent_at=excluded.sent_at`,
			chunk.ProfileID, candidate.LineID, candidate.Generation, candidate.WindowInstance, candidate.Fingerprint, boolInt(candidate.Good), boolInt(candidate.Unknown), candidate.Description, candidate.Reason, candidate.ObservedAt, candidate.SeverityKey, messageID, sentAt.UTC().UnixMilli())
		if err != nil {
			return fmt.Errorf("save line delivery receipt: %w", err)
		}
		return nil
	})
}

// RetireEditTarget persists a confirmed unusable target before attempting its replacement.
// It keeps the operation version so that the immediate replacement can complete it.
func (s *Store) RetireEditTarget(ctx context.Context, chunk OutboxChunk) error {
	if len(chunk.Candidates) != 1 || len(chunk.LineIDs) != 1 || chunk.Candidates[0].LineID != chunk.LineIDs[0] || chunk.Candidates[0].Generation != chunk.Generation || chunk.MessageID <= 0 {
		return errors.New("retire edit target: mismatched candidate or target")
	}
	return s.write(ctx, func(conn *sql.Conn) error {
		candidate := chunk.Candidates[0]
		result, err := conn.ExecContext(ctx, `UPDATE outbox_chunks SET operation='send',message_id=0
   WHERE id=? AND version=? AND profile_id=? AND generation=? AND window_instance=? AND event_type='status' AND operation='edit' AND message_id=?
    AND EXISTS (SELECT 1 FROM profiles WHERE id=outbox_chunks.profile_id AND generation=? AND enabled=1)`,
			chunk.ID, chunk.Version, chunk.ProfileID, chunk.Generation, candidate.WindowInstance, chunk.MessageID, chunk.Generation)
		if err != nil {
			return fmt.Errorf("retire pending edit target: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check retired edit target: %w", err)
		}
		if count == 0 {
			return nil
		}
		if _, err := conn.ExecContext(ctx, `UPDATE monitor_states SET message_id=0 WHERE profile_id=? AND line_id=? AND generation=? AND window_instance=? AND message_id=?`,
			chunk.ProfileID, candidate.LineID, chunk.Generation, candidate.WindowInstance, chunk.MessageID); err != nil {
			return fmt.Errorf("clear retired line receipt: %w", err)
		}
		return nil
	})
}

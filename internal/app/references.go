package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/motohedgehog/edi/internal/logging"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

type referenceStore interface {
	LoadCatalog(context.Context) ([]store.CatalogLine, error)
	SaveCatalog(context.Context, []store.CatalogLine) error
	LoadSeverities(context.Context) ([]store.SeverityRecord, error)
	SaveSeverities(context.Context, []store.SeverityRecord) error
}

type referenceClient interface {
	Catalog(context.Context) ([]tfl.Line, error)
	Severities(context.Context) ([]tfl.Severity, error)
	SetSeverities([]tfl.Severity) error
}

// references owns cache loading, refresh, and publication of the current catalog.
type references struct {
	store  referenceStore
	client referenceClient
	logger *slog.Logger

	refreshMu     sync.Mutex
	catalogMu     sync.RWMutex
	catalog       []tfl.Line
	refreshFailed bool
}

func newReferences(repository referenceStore, client referenceClient, logger *slog.Logger) *references {
	if logger == nil {
		logger = slog.Default()
	}
	return &references{store: repository, client: client, logger: logger.With("component", "references")}
}

// Initialize publishes usable cached values before attempting independent refreshes.
func (r *references) Initialize(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	if cached, err := r.store.LoadCatalog(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.WarnContext(ctx, "Could not load cached TfL line catalog", "operation", "load_catalog", logging.Error(err))
		}
	} else if len(cached) > 0 {
		lines := make([]tfl.Line, 0, len(cached))
		for _, line := range cached {
			lines = append(lines, tfl.Line{ID: line.ID, DisplayName: line.DisplayName, ModeName: line.ModeName})
		}
		r.publishCatalog(lines)
		r.logger.InfoContext(ctx, "Loaded cached TfL line catalog", "operation", "catalog", "count", len(lines))
	}

	if cached, err := r.store.LoadSeverities(ctx); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.WarnContext(ctx, "Could not load cached TfL severity metadata", "operation", "load_severities", logging.Error(err))
		}
	} else if len(cached) > 0 {
		values := make([]tfl.Severity, 0, len(cached))
		for _, value := range cached {
			values = append(values, tfl.Severity{ModeName: value.ModeName, SeverityLevel: value.SeverityLevel, Description: value.Description})
		}
		if err := r.client.SetSeverities(values); err != nil {
			if !errors.Is(err, context.Canceled) {
				r.logger.WarnContext(ctx, "Could not apply cached TfL severity metadata", "operation", "apply_severities", logging.Error(err))
			}
		} else {
			r.logger.InfoContext(ctx, "Loaded cached TfL severity metadata", "operation", "severities", "count", len(values))
		}
	}

	return r.refresh(ctx)
}

// Refresh updates each reference independently and retains the last usable catalog on failure.
func (r *references) Refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	return r.refresh(ctx)
}

func (r *references) refresh(ctx context.Context) error {
	started := time.Now()
	r.logger.DebugContext(ctx, "Refreshing TfL reference data", "operation", "refresh")
	catalogErr := r.refreshCatalog(ctx)
	severityErr := r.refreshSeverities(ctx)
	err := errors.Join(catalogErr, severityErr)
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil {
		r.refreshFailed = true
		r.logger.WarnContext(ctx, "TfL reference data could not be fully refreshed", "operation", "refresh", "catalog_available", len(r.Catalog()) > 0, "catalog_refreshed", catalogErr == nil, "severities_refreshed", severityErr == nil, "duration_ms", time.Since(started).Milliseconds(), logging.Error(err))
	} else {
		if r.refreshFailed {
			r.logger.InfoContext(ctx, "TfL reference data refresh recovered", "operation", "refresh")
		}
		r.refreshFailed = false
		r.logger.InfoContext(ctx, "TfL reference data refreshed", "operation", "refresh", "catalog_count", len(r.Catalog()), "duration_ms", time.Since(started).Milliseconds())
	}
	return err
}

func (r *references) refreshCatalog(ctx context.Context) error {
	lines, err := r.client.Catalog(ctx)
	if err != nil {
		return fmt.Errorf("refresh TfL catalog: %w", err)
	}
	r.publishCatalog(lines)

	records := make([]store.CatalogLine, 0, len(lines))
	updatedAt := time.Now().UTC()
	for _, line := range lines {
		records = append(records, store.CatalogLine{ID: line.ID, DisplayName: line.DisplayName, ModeName: line.ModeName, UpdatedAt: updatedAt})
	}
	if err := r.store.SaveCatalog(ctx, records); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.WarnContext(ctx, "Could not cache refreshed TfL line catalog", "operation", "save_catalog", "outcome", "memory_retained", logging.Error(err))
		}
	}
	return nil
}

func (r *references) refreshSeverities(ctx context.Context) error {
	values, err := r.client.Severities(ctx)
	if err != nil {
		return fmt.Errorf("refresh TfL severity metadata: %w", err)
	}
	records := make([]store.SeverityRecord, 0, len(values))
	updatedAt := time.Now().UTC()
	for _, value := range values {
		records = append(records, store.SeverityRecord{ModeName: value.ModeName, SeverityLevel: value.SeverityLevel, Description: value.Description, UpdatedAt: updatedAt})
	}
	if err := r.store.SaveSeverities(ctx, records); err != nil {
		if !errors.Is(err, context.Canceled) {
			r.logger.WarnContext(ctx, "Could not cache refreshed TfL severity metadata", "operation", "save_severities", "outcome", "memory_retained", logging.Error(err))
		}
	}
	return nil
}

func (r *references) Catalog() []tfl.Line {
	r.catalogMu.RLock()
	defer r.catalogMu.RUnlock()
	return append([]tfl.Line(nil), r.catalog...)
}

func (r *references) publishCatalog(lines []tfl.Line) {
	snapshot := append([]tfl.Line(nil), lines...)
	r.catalogMu.Lock()
	r.catalog = snapshot
	r.catalogMu.Unlock()
}

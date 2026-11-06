package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/motohedgehog/edi/internal/logging"
)

type ownedWorker struct {
	name string
	run  func(context.Context) error
}

func runWorkers(parent context.Context, logger *slog.Logger, workers ...ownedWorker) error {
	if len(workers) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	results := make(chan workerFailure, len(workers))
	for _, worker := range workers {
		go func(worker ownedWorker) {
			results <- workerFailure{name: worker.name, err: runOwnedWorker(ctx, logger, worker)}
		}(worker)
	}

	remaining := len(workers)
	var firstErr error
	for remaining > 0 {
		result := <-results
		remaining--
		if ctx.Err() == nil {
			if result.err == nil {
				result.err = logging.Message("worker stopped unexpectedly")
			}
			firstErr = result
			cancel()
			continue
		}
		if firstErr == nil && result.err != nil && !errors.Is(result.err, context.Canceled) && !errors.Is(result.err, context.DeadlineExceeded) {
			firstErr = result
		}
	}
	return firstErr
}

func runOwnedWorker(ctx context.Context, logger *slog.Logger, worker ownedWorker) (err error) {
	logger.DebugContext(ctx, "Application worker started", "operation", "run", "worker", worker.name)
	defer func() {
		logger.DebugContext(ctx, "Application worker stopped", "operation", "run", "worker", worker.name)
		if recover() != nil {
			err = workerPanic{name: worker.name}
		}
	}()
	return worker.run(ctx)
}

type workerFailure struct {
	name string
	err  error
}

func (e workerFailure) Error() string      { return fmt.Sprintf("%s worker failed: %v", e.name, e.err) }
func (e workerFailure) Unwrap() error      { return e.err }
func (e workerFailure) WorkerName() string { return e.name }

type workerPanic struct{ name string }

func (e workerPanic) Error() string { return e.name + " worker panicked" }

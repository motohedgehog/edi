package telegram

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/motohedgehog/edi/internal/logging"
)

type lifecycleTransport struct {
	base http.RoundTripper
	mu   sync.RWMutex
	ctx  context.Context
}

func (t *lifecycleTransport) BindContext(ctx context.Context) {
	t.mu.Lock()
	t.ctx = ctx
	t.mu.Unlock()
}

func (t *lifecycleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.RLock()
	boundCtx := t.ctx
	t.mu.RUnlock()
	if err := boundCtx.Err(); err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithCancel(request.Context())
	stop := context.AfterFunc(boundCtx, cancel)
	response, err := t.base.RoundTrip(request.WithContext(requestCtx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if response == nil {
		stop()
		cancel()
		return nil, logging.Message("telegram HTTP transport returned an empty response")
	}
	if response.Body != nil {
		response.Body = &lifecycleResponseBody{ReadCloser: response.Body, stop: stop, cancel: cancel}
	} else {
		stop()
		cancel()
		response.Body = http.NoBody
	}
	return response, nil
}

type lifecycleResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
	once   sync.Once
}

func (b *lifecycleResponseBody) Read(content []byte) (int, error) {
	n, err := b.ReadCloser.Read(content)
	if err != nil {
		_ = b.Close()
	}
	return n, err
}

func (b *lifecycleResponseBody) Close() error {
	var err error
	b.once.Do(func() {
		err = b.ReadCloser.Close()
		b.stop()
		b.cancel()
	})
	return err
}

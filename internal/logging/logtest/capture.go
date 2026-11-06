// Package logtest provides concurrent log capture for offline tests.
package logtest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type Capture struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func New(level slog.Level) (*slog.Logger, *Capture) {
	capture := &Capture{}
	return slog.New(slog.NewJSONHandler(capture, &slog.HandlerOptions{Level: level})), capture
}

func (c *Capture) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffer.Write(data)
}

func (c *Capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffer.String()
}

func (c *Capture) Records(t *testing.T) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(c.String(), "\n") {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func (c *Capture) AssertPrivate(t *testing.T, markers ...string) {
	t.Helper()
	for _, marker := range markers {
		if bytes.Contains([]byte(c.String()), []byte(marker)) {
			t.Errorf("private marker %q was logged", marker)
		}
	}
	for _, record := range c.Records(t) {
		if record["level"] != "DEBUG" && (record["user_id"] != nil || record["chat_id"] != nil) {
			t.Errorf("identifier above DEBUG: %v", record)
		}
	}
}

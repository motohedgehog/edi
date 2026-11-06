// Package tfl contains the small TfL API client and status normalization rules.
package tfl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/motohedgehog/edi/internal/logging"
)

const (
	BaseURL        = "https://api.tfl.gov.uk"
	supportedModes = "tube,dlr,elizabeth-line,overground"
	defaultAgent   = "edi/dev"
	agentProduct   = "edi"
	MaxBodyBytes   = 4 << 20
	MaxAttempts    = 3
	GoodSeverity   = 10
	maxRetryAfter  = 10 * time.Second
)

type Line struct {
	ID          string `json:"id"`
	DisplayName string `json:"name"`
	ModeName    string `json:"modeName"`
}

type Severity struct {
	ModeName      string `json:"modeName"`
	SeverityLevel int    `json:"severityLevel"`
	Description   string `json:"description"`
}

type NormalizedStatus struct {
	Severity    int
	Description string
	Reason      string
	Known       bool
}

type LineStatus struct {
	LineID      string
	DisplayName string
	Statuses    []NormalizedStatus
	Fingerprint string
	Good        bool
	Unknown     bool
}

type Snapshot struct {
	ObservedAt time.Time
	Lines      map[string]LineStatus
	Partial    bool
	Warnings   int
}

type StatusError struct {
	Code      int
	Retryable bool
	Message   string
}

func (e *StatusError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("TfL API returned HTTP %d", e.Code)
	}
	return e.Message
}

type Client struct {
	logger     *slog.Logger
	failures   map[string]int
	httpClient *http.Client
	baseURL    string
	appKey     string
	userAgent  string
	sleep      func(context.Context, time.Duration) error
	jitter     func(time.Duration) time.Duration
	mu         sync.RWMutex
	severities map[string]map[int]struct{}
}

type Option func(*Client)

func WithLogger(logger *slog.Logger) Option {
	return func(c *Client) {
		if logger != nil {
			c.logger = logger.With("component", "tfl")
		}
	}
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithBaseURL is intended for deterministic tests; production uses the fixed TfL HTTPS URL.
func WithBaseURL(value string) Option {
	return func(c *Client) {
		if strings.TrimRight(value, "/") != "" {
			c.baseURL = strings.TrimRight(value, "/")
		}
	}
}

// WithVersion sets the application version included in the User-Agent header.
func WithVersion(version string) Option {
	return func(c *Client) {
		version = strings.TrimSpace(version)
		if version == "" || strings.ContainsAny(version, "\r\n") {
			return
		}
		c.userAgent = agentProduct + "/" + version
	}
}

func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

func WithJitter(jitter func(time.Duration) time.Duration) Option {
	return func(c *Client) {
		if jitter != nil {
			c.jitter = jitter
		}
	}
}

func NewClient(appKey string, timeout time.Duration, opts ...Option) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &Client{
		baseURL:   BaseURL,
		logger:    slog.Default().With("component", "tfl"),
		failures:  make(map[string]int),
		appKey:    appKey,
		userAgent: defaultAgent,
		httpClient: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		severities: map[string]map[int]struct{}{"tube": {GoodSeverity: {}}},
	}
	client.sleep = sleepContext
	client.jitter = func(limit time.Duration) time.Duration {
		if limit <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(limit) + 1))
	}
	for _, option := range opts {
		option(client)
	}
	return client
}

func (c *Client) SetSeverities(values []Severity) error {
	parsed, err := ValidateSeverities(values)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.severities = parsed
	c.mu.Unlock()
	return nil
}

func (c *Client) Catalog(ctx context.Context) (result []Line, err error) {
	start := time.Now()
	defer func() { c.observe(ctx, "catalog", start, len(result), err) }()
	var lines []Line
	if err := c.getJSON(ctx, "catalog", "/Line/Mode/"+supportedModes, &lines); err != nil {
		return nil, fmt.Errorf("fetch TfL line catalog: %w", err)
	}
	valid := make([]Line, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		line.ID = normalizeKey(line.ID)
		line.DisplayName = normalizeText(line.DisplayName)
		line.ModeName = normalizeKey(line.ModeName)
		if !supportedMode(line.ModeName) {
			continue
		}
		if line.ID == "" || line.DisplayName == "" {
			continue
		}
		if _, exists := seen[line.ID]; exists {
			continue
		}
		seen[line.ID] = struct{}{}
		valid = append(valid, line)
	}
	if len(valid) == 0 {
		return nil, logging.Message("TfL line catalog is empty")
	}
	sortLines(valid)
	return valid, nil
}

func (c *Client) Severities(ctx context.Context) (result []Severity, err error) {
	start := time.Now()
	defer func() { c.observe(ctx, "severities", start, len(result), err) }()
	var values []Severity
	if err := c.getJSON(ctx, "severities", "/Line/Meta/Severity", &values); err != nil {
		return nil, fmt.Errorf("fetch TfL severity metadata: %w", err)
	}
	if err := c.SetSeverities(values); err != nil {
		return nil, err
	}
	return values, nil
}

func (c *Client) Status(ctx context.Context) (result Snapshot, err error) {
	start := time.Now()
	defer func() { c.observe(ctx, "status", start, len(result.Lines), err) }()
	var raw []json.RawMessage
	if err := c.getJSON(ctx, "status", "/Line/Mode/"+supportedModes+"/Status", &raw); err != nil {
		return Snapshot{}, fmt.Errorf("fetch TfL status: %w", err)
	}
	if len(raw) == 0 {
		return Snapshot{}, logging.Message("TfL status response is empty")
	}
	observedAt := time.Now().UTC()
	snapshot := normalizeRaw(raw, observedAt, c.knownSeverities())
	if snapshot.HasUnknownSeverity() {
		if _, err := c.Severities(ctx); err == nil {
			snapshot = normalizeRaw(raw, observedAt, c.knownSeverities())
		}
	}
	if snapshot.Partial || snapshot.HasUnknownSeverity() {
		c.logger.WarnContext(ctx, "TfL status response is incomplete or has unknown severities", "operation", "status", "count", len(snapshot.Lines), "invalid_count", snapshot.Warnings, "unknown_severity", snapshot.HasUnknownSeverity())
	}
	return snapshot, nil
}

func (s Snapshot) HasUnknownSeverity() bool {
	for _, line := range s.Lines {
		for _, status := range line.Statuses {
			if !status.Known {
				return true
			}
		}
	}
	return false
}

func (c *Client) knownSeverities() map[string]map[int]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[string]map[int]struct{}, len(c.severities))
	for mode, values := range c.severities {
		result[mode] = make(map[int]struct{}, len(values))
		for value := range values {
			result[mode][value] = struct{}{}
		}
	}
	return result
}

func (c *Client) getJSON(ctx context.Context, operation, path string, target any) error {
	requestURL, err := url.Parse(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("build TfL request: %w", err)
	}
	query := requestURL.Query()
	if c.appKey != "" {
		query.Set("app_key", c.appKey)
	}
	requestURL.RawQuery = query.Encode()

	var lastErr error
	for attempt := range MaxAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
		if err != nil {
			return fmt.Errorf("build TfL request: %w", err)
		}
		c.logger.DebugContext(ctx, "Requesting TfL data", "operation", operation, "attempt", attempt+1)
		request.Header.Set("User-Agent", c.userAgent)
		response, requestErr := c.httpClient.Do(request)
		if requestErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = requestErr
			if attempt+1 < MaxAttempts {
				if err := c.wait(ctx, operation, attempt, 0, requestErr); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("TfL request failed after %d attempts: %w", MaxAttempts, lastErr)
		}

		body, readErr := readLimited(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read TfL response: %w", readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close TfL response: %w", closeErr)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			apiErr := &StatusError{Code: response.StatusCode, Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500}
			lastErr = apiErr
			if !apiErr.Retryable || attempt+1 == MaxAttempts {
				return apiErr
			}
			if err := c.wait(ctx, operation, attempt, retryAfter(response.Header), apiErr); err != nil {
				return err
			}
			continue
		}
		if !jsonContentType(response.Header.Get("Content-Type")) {
			return logging.Message("TfL response has unsupported content type")
		}
		if err := json.Unmarshal(body, target); err != nil {
			return fmt.Errorf("decode TfL response: %w", err)
		}
		return nil
	}
	return lastErr
}

func readLimited(body io.Reader) ([]byte, error) {
	limited := io.LimitReader(body, MaxBodyBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxBodyBytes {
		return nil, logging.Message("TfL response exceeds 4 MiB")
	}
	return data, nil
}

func jsonContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return contentType == "application/json" || strings.HasSuffix(contentType, "+json")
}

func retryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(time.Until(at), maxRetryAfter)
	}
	return 0
}

func (c *Client) wait(ctx context.Context, operation string, attempt int, serverDelay time.Duration, err error) error {
	backoff := 500 * time.Millisecond
	if attempt > 0 {
		backoff = time.Second
	}
	if serverDelay > 0 {
		backoff = serverDelay
	}
	backoff = min(backoff+c.jitter(backoff/5), maxRetryAfter)
	c.logger.DebugContext(ctx, "Waiting to retry TfL request", "operation", operation, "attempt", attempt+1, "retry_delay_ms", backoff.Milliseconds(), logging.Error(err))
	return c.sleep(ctx, backoff)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func normalizeRaw(raw []json.RawMessage, observedAt time.Time, known map[string]map[int]struct{}) Snapshot {
	snapshot := Snapshot{ObservedAt: observedAt.UTC(), Lines: make(map[string]LineStatus)}
	for _, entry := range raw {
		var wire struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			ModeName     string `json:"modeName"`
			LineStatuses []struct {
				StatusSeverity            *int   `json:"statusSeverity"`
				StatusSeverityDescription string `json:"statusSeverityDescription"`
				Reason                    string `json:"reason"`
			} `json:"lineStatuses"`
		}
		if err := json.Unmarshal(entry, &wire); err != nil {
			var partial struct {
				ID string `json:"id"`
			}
			if partialErr := json.Unmarshal(entry, &partial); partialErr == nil && normalizeKey(partial.ID) != "" {
				id := normalizeKey(partial.ID)
				snapshot.Lines[id] = unknownLine(id, "", "invalid line data")
			}
			snapshot.Partial = true
			snapshot.Warnings++
			continue
		}
		id := normalizeKey(wire.ID)
		if id == "" {
			snapshot.Partial = true
			snapshot.Warnings++
			continue
		}
		if _, duplicate := snapshot.Lines[id]; duplicate {
			snapshot.Lines[id] = unknownLine(id, normalizeText(wire.Name), "duplicate line ID")
			snapshot.Partial = true
			snapshot.Warnings++
			continue
		}
		if len(wire.LineStatuses) == 0 {
			snapshot.Lines[id] = unknownLine(id, normalizeText(wire.Name), "empty status list")
			snapshot.Partial = true
			snapshot.Warnings++
			continue
		}
		mode := normalizeKey(wire.ModeName)
		statuses := make([]NormalizedStatus, 0, len(wire.LineStatuses))
		seen := make(map[string]struct{})
		for _, item := range wire.LineStatuses {
			severity := 0
			if item.StatusSeverity != nil {
				severity = *item.StatusSeverity
			}
			description := normalizeText(item.StatusSeverityDescription)
			reason := normalizeText(item.Reason)
			isKnown := false
			if values, ok := known[mode]; ok {
				_, isKnown = values[severity]
			}
			status := NormalizedStatus{Severity: severity, Description: description, Reason: reason, Known: isKnown}
			key := statusKey(status)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			statuses = append(statuses, status)
		}
		if len(statuses) == 0 {
			snapshot.Lines[id] = unknownLine(id, normalizeText(wire.Name), "empty status list")
			snapshot.Partial = true
			snapshot.Warnings++
			continue
		}
		sortStatuses(statuses)
		good := true
		unknown := false
		for _, status := range statuses {
			if !status.Known {
				good = false
				unknown = true
				continue
			}
			if status.Severity != GoodSeverity {
				good = false
			}
		}
		snapshot.Lines[id] = LineStatus{LineID: id, DisplayName: normalizeText(wire.Name), Statuses: statuses,
			Fingerprint: fingerprint(statuses), Good: good, Unknown: unknown}
	}
	return snapshot
}

func unknownLine(id, name, reason string) LineStatus {
	status := NormalizedStatus{Severity: 0, Description: "Unknown", Reason: normalizeText(reason), Known: false}
	return LineStatus{LineID: id, DisplayName: name, Statuses: []NormalizedStatus{status}, Fingerprint: fingerprint([]NormalizedStatus{status}), Unknown: true}
}

// ForSelection completes a snapshot for selected IDs, marking absent or malformed lines unknown.
func ForSelection(snapshot Snapshot, selected []string, catalog []Line) map[string]LineStatus {
	byID := make(map[string]Line, len(catalog))
	for _, line := range catalog {
		byID[normalizeKey(line.ID)] = line
	}
	result := make(map[string]LineStatus, len(selected))
	for _, selectedID := range uniqueKeys(selected) {
		id := normalizeKey(selectedID)
		if current, ok := snapshot.Lines[id]; ok {
			if current.DisplayName == "" {
				current.DisplayName = byID[id].DisplayName
			}
			result[id] = current
			continue
		}
		result[id] = unknownLine(id, byID[id].DisplayName, "line missing from response")
	}
	return result
}

func fingerprint(statuses []NormalizedStatus) string {
	var builder strings.Builder
	for _, status := range statuses {
		builder.WriteString(strconv.Itoa(status.Severity))
		builder.WriteByte('|')
		builder.WriteString(strings.ToLower(status.Description))
		builder.WriteByte('|')
		builder.WriteString(strings.ToLower(status.Reason))
		builder.WriteByte('|')
		if status.Known {
			builder.WriteByte('1')
		} else {
			builder.WriteByte('0')
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

func statusKey(status NormalizedStatus) string {
	return fmt.Sprintf("%d\x00%s\x00%s", status.Severity, strings.ToLower(status.Description), strings.ToLower(status.Reason))
}

func normalizeText(value string) string {
	return strings.Join(strings.FieldsFunc(strings.TrimSpace(value), unicode.IsSpace), " ")
}

func normalizeKey(value string) string { return strings.ToLower(normalizeText(value)) }

func supportedMode(mode string) bool {
	return slices.Contains(strings.Split(supportedModes, ","), mode)
}

func sortStatuses(statuses []NormalizedStatus) {
	sort.Slice(statuses, func(i, j int) bool {
		if statuses[i].Severity != statuses[j].Severity {
			return statuses[i].Severity < statuses[j].Severity
		}
		if !strings.EqualFold(statuses[i].Description, statuses[j].Description) {
			return strings.ToLower(statuses[i].Description) < strings.ToLower(statuses[j].Description)
		}
		return strings.ToLower(statuses[i].Reason) < strings.ToLower(statuses[j].Reason)
	})
}

func sortLines(lines []Line) {
	sort.Slice(lines, func(i, j int) bool {
		left, right := strings.ToLower(lines[i].DisplayName), strings.ToLower(lines[j].DisplayName)
		if left != right {
			return left < right
		}
		return lines[i].ID < lines[j].ID
	})
}

func uniqueKeys(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeKey(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// ValidateSeverities verifies the metadata contract and returns a normalized lookup.
func ValidateSeverities(values []Severity) (map[string]map[int]struct{}, error) {
	if len(values) == 0 {
		return nil, logging.Message("severity metadata is empty")
	}
	lookup := make(map[string]map[int]struct{})
	for _, value := range values {
		mode := normalizeKey(value.ModeName)
		description := normalizeText(value.Description)
		if mode == "" || description == "" {
			return nil, logging.Message("severity metadata contains an empty field")
		}
		if lookup[mode] == nil {
			lookup[mode] = make(map[int]struct{})
		}
		if _, exists := lookup[mode][value.SeverityLevel]; exists {
			return nil, logging.Message("severity metadata contains duplicate values")
		}
		lookup[mode][value.SeverityLevel] = struct{}{}
	}
	if _, ok := lookup["tube"][GoodSeverity]; !ok {
		return nil, logging.Message("severity metadata does not contain tube severity 10")
	}
	return lookup, nil
}

func (c *Client) observe(ctx context.Context, operation string, started time.Time, count int, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	c.mu.Lock()
	previous := c.failures[operation]
	if err != nil {
		c.failures[operation]++
	} else {
		c.failures[operation] = 0
	}
	failures := c.failures[operation]
	c.mu.Unlock()
	if err != nil {
		level := slog.LevelWarn
		var status *StatusError
		if errors.As(err, &status) && !status.Retryable {
			level = slog.LevelError
		}
		c.logger.Log(ctx, level, "Could not fetch TfL data", "operation", operation, "duration_ms", time.Since(started).Milliseconds(), "failure_count", failures, logging.Error(err))
		return
	}
	if previous > 0 {
		c.logger.InfoContext(ctx, "TfL data fetching recovered", "operation", operation, "failure_count", previous)
	}
	c.logger.DebugContext(ctx, "TfL data fetched", "operation", operation, "outcome", "success", "duration_ms", time.Since(started).Milliseconds(), "count", count)
}

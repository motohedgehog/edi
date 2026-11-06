package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

type referenceStoreFake struct {
	catalog        []store.CatalogLine
	severities     []store.SeverityRecord
	loadCatalogErr error
	loadSevErr     error
	saveCatalogErr error
	saveSevErr     error
}

func (s *referenceStoreFake) LoadCatalog(context.Context) ([]store.CatalogLine, error) {
	return append([]store.CatalogLine(nil), s.catalog...), s.loadCatalogErr
}

func (s *referenceStoreFake) SaveCatalog(_ context.Context, lines []store.CatalogLine) error {
	if s.saveCatalogErr == nil {
		s.catalog = append([]store.CatalogLine(nil), lines...)
	}
	return s.saveCatalogErr
}

func (s *referenceStoreFake) LoadSeverities(context.Context) ([]store.SeverityRecord, error) {
	return append([]store.SeverityRecord(nil), s.severities...), s.loadSevErr
}

func (s *referenceStoreFake) SaveSeverities(_ context.Context, values []store.SeverityRecord) error {
	if s.saveSevErr == nil {
		s.severities = append([]store.SeverityRecord(nil), values...)
	}
	return s.saveSevErr
}

type referenceClientFake struct {
	catalog       []tfl.Line
	catalogErr    error
	severities    []tfl.Severity
	severitiesErr error
	setErr        error
	current       []tfl.Severity
}

func (c *referenceClientFake) Catalog(context.Context) ([]tfl.Line, error) {
	return append([]tfl.Line(nil), c.catalog...), c.catalogErr
}

func (c *referenceClientFake) Severities(context.Context) ([]tfl.Severity, error) {
	if c.severitiesErr != nil {
		return nil, c.severitiesErr
	}
	c.current = append([]tfl.Severity(nil), c.severities...)
	return append([]tfl.Severity(nil), c.severities...), nil
}

func (c *referenceClientFake) SetSeverities(values []tfl.Severity) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.current = append([]tfl.Severity(nil), values...)
	return nil
}

func newReferenceTestModule(repository *referenceStoreFake, client *referenceClientFake, logs *bytes.Buffer) *references {
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return newReferences(repository, client, logger)
}

func TestReferencesInitializeFromEmptyCacheAndRecoverOnRefresh(t *testing.T) {
	repository := &referenceStoreFake{}
	client := &referenceClientFake{catalogErr: errors.New("catalog offline"), severitiesErr: errors.New("severities offline")}
	var logs bytes.Buffer
	refs := newReferenceTestModule(repository, client, &logs)
	if err := refs.Initialize(context.Background()); err == nil {
		t.Fatal("Initialize succeeded while both remote reference requests failed")
	}
	if got := refs.Catalog(); len(got) != 0 {
		t.Fatalf("empty startup catalog = %+v", got)
	}

	client.catalogErr = nil
	client.catalog = []tfl.Line{{ID: "central", DisplayName: "Central"}}
	client.severitiesErr = nil
	client.severities = []tfl.Severity{{ModeName: "tube", SeverityLevel: 5, Description: "minor"}, {ModeName: "tube", SeverityLevel: 10, Description: "good"}}
	if err := refs.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := refs.Catalog(); len(got) != 1 || got[0].ID != "central" {
		t.Fatalf("recovered catalog = %+v", got)
	}
}

func TestReferencesUseCachedDataAndRetainItWhenRefreshFails(t *testing.T) {
	repository := &referenceStoreFake{
		catalog: []store.CatalogLine{{ID: "central", DisplayName: "Cached Central", ModeName: "tube"}},
		severities: []store.SeverityRecord{{ModeName: "tube", SeverityLevel: 5, Description: "cached minor"},
			{ModeName: "tube", SeverityLevel: 10, Description: "cached good"}},
	}
	client := &referenceClientFake{catalogErr: errors.New("catalog offline"), severitiesErr: errors.New("severity offline")}
	var logs bytes.Buffer
	refs := newReferenceTestModule(repository, client, &logs)
	if err := refs.Initialize(context.Background()); err == nil {
		t.Fatal("Initialize succeeded while remote reference requests failed")
	}
	got := refs.Catalog()
	if len(got) != 1 || got[0].DisplayName != "Cached Central" {
		t.Fatalf("cached catalog was not published: %+v", got)
	}
	if len(client.current) != 2 || client.current[0].Description != "cached minor" {
		t.Fatalf("cached severities were not loaded into the client: %+v", client.current)
	}
	if err := refs.Refresh(context.Background()); err == nil {
		t.Fatal("failed refresh returned nil")
	}
	if got := refs.Catalog(); len(got) != 1 || got[0].DisplayName != "Cached Central" {
		t.Fatalf("failed refresh discarded cached catalog: %+v", got)
	}
	if len(client.current) != 2 || client.current[0].Description != "cached minor" {
		t.Fatalf("failed severity refresh replaced cached metadata: %+v", client.current)
	}
}

func TestReferencesKeepFreshCatalogWhenCacheWriteFails(t *testing.T) {
	repository := &referenceStoreFake{saveCatalogErr: errors.New("cache read only"), saveSevErr: errors.New("cache read only")}
	client := &referenceClientFake{
		catalog: []tfl.Line{{ID: "central", DisplayName: "Fresh Central"}},
		severities: []tfl.Severity{{ModeName: "tube", SeverityLevel: 5, Description: "minor"},
			{ModeName: "tube", SeverityLevel: 10, Description: "good"}},
	}
	var logs bytes.Buffer
	refs := newReferenceTestModule(repository, client, &logs)
	if err := refs.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := refs.Catalog(); len(got) != 1 || got[0].DisplayName != "Fresh Central" {
		t.Fatalf("cache write failure discarded fresh catalog: %+v", got)
	}
	if !bytes.Contains(logs.Bytes(), []byte("operation=save_catalog")) {
		t.Fatalf("catalog cache failure was not reported: %s", logs.String())
	}
}

func TestReferencesCatalogReturnsDefensiveCopies(t *testing.T) {
	client := &referenceClientFake{catalog: []tfl.Line{{ID: "central", DisplayName: "Central"}}}
	refs := newReferenceTestModule(&referenceStoreFake{}, client, &bytes.Buffer{})
	if err := refs.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := refs.Catalog()
	first[0].DisplayName = "changed"
	first[0] = tfl.Line{ID: "victoria", DisplayName: "Victoria"}
	if got := refs.Catalog(); len(got) != 1 || got[0].ID != "central" || got[0].DisplayName != "Central" {
		t.Fatalf("consumer mutated published catalog: %+v", got)
	}
}

func TestReferencesConcurrentRefreshAndRead(t *testing.T) {
	client := &referenceClientFake{catalog: []tfl.Line{{ID: "central", DisplayName: "Central"}}}
	refs := newReferenceTestModule(&referenceStoreFake{}, client, &bytes.Buffer{})
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			for range 25 {
				if err := refs.Refresh(context.Background()); err != nil {
					t.Errorf("refresh: %v", err)
				}
			}
		})
		group.Go(func() {
			for range 25 {
				if got := refs.Catalog(); len(got) > 1 {
					t.Errorf("catalog snapshot has %d lines", len(got))
				}
			}
		})
	}
	group.Wait()
}

func TestManualCheckerUsesCurrentCatalogForEachCheck(t *testing.T) {
	transport := appRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/Line/Mode/tube,dlr,elizabeth-line,overground/Status" {
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`[{"id":"central","modeName":"tube","lineStatuses":[{"statusSeverity":10,"statusSeverityDescription":"Good Service"}]}]`)), Request: request}, nil
	})
	client := tfl.NewClient("", time.Second, tfl.WithBaseURL("https://api.tfl.gov.uk"), tfl.WithHTTPClient(&http.Client{Transport: transport}))
	catalogName := "Central First"
	checker := &manualChecker{client: client, catalog: func() []tfl.Line {
		return []tfl.Line{{ID: "central", DisplayName: catalogName}}
	}, clock: monitor.RealClock{}}
	check := func() string {
		t.Helper()
		chunks, err := checker.Check(context.Background(), []string{"central"}, "Europe/London")
		if err != nil || len(chunks) == 0 {
			t.Fatalf("manual check chunks=%+v err=%v", chunks, err)
		}
		return chunks[0].Body
	}
	if body := check(); !strings.Contains(body, "Central First") {
		t.Fatalf("first manual check catalog name = %q", body)
	}
	catalogName = "Central Fresh"
	if body := check(); !strings.Contains(body, "Central Fresh") {
		t.Fatalf("later manual check used stale catalog name: %q", body)
	}
}

type appRoundTripFunc func(*http.Request) (*http.Response, error)

func (f appRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

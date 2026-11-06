package tfl_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/motohedgehog/edi/internal/tfl"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func response(status int, body string, contentType string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}},
		Body: io.NopCloser(strings.NewReader(body)), Request: &http.Request{}}
}

func TestClientUsesRoutesAndEncodedAppKey(t *testing.T) {
	var seenPath, seenQuery, seenUserAgent string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seenPath, seenQuery = r.URL.Path, r.URL.RawQuery
		seenUserAgent = r.Header.Get("User-Agent")
		return response(http.StatusOK, `[{"id":"central","name":"Central","modeName":"tube"}]`, "application/json; charset=utf-8"), nil
	})
	client := tfl.NewClient("key with spaces", time.Second, tfl.WithBaseURL("https://api.tfl.gov.uk"), tfl.WithVersion("1.2.3"), tfl.WithHTTPClient(&http.Client{Transport: transport}), tfl.WithJitter(func(time.Duration) time.Duration { return 0 }))
	lines, err := client.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].ID != "central" || seenPath != "/Line/Mode/tube,dlr,elizabeth-line,overground" || !strings.Contains(seenQuery, "app_key=key+with+spaces") || seenUserAgent != "edi/1.2.3" {
		t.Fatalf("unexpected request/result: path=%q query=%q user_agent=%q lines=%+v", seenPath, seenQuery, seenUserAgent, lines)
	}
}

func TestClientRetriesServerFailure(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if calls.Add(1) < 3 {
			return response(http.StatusServiceUnavailable, "", ""), nil
		}
		return response(http.StatusOK, `[{"id":"central","name":"Central","modeName":"tube"}]`, "application/json"), nil
	})
	client := tfl.NewClient("", time.Second, tfl.WithBaseURL("https://api.tfl.gov.uk"), tfl.WithHTTPClient(&http.Client{Transport: transport}),
		tfl.WithJitter(func(time.Duration) time.Duration { return 0 }),
		tfl.WithSleep(func(context.Context, time.Duration) error { return nil }))
	if _, err := client.Catalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("got %d attempts, want 3", calls.Load())
	}
}

func TestStatusNormalizationAndPartialSelection(t *testing.T) {
	var raw = `[
{"id":"central","name":" Central ","modeName":"tube","lineStatuses":[{"statusSeverity":5,"statusSeverityDescription":"  Minor   delays ","reason":" trains  are slow "},{"statusSeverity":5,"statusSeverityDescription":"Minor delays","reason":"trains are slow"}]},
{"id":"bad","lineStatuses":[]},
{"id":"broken","lineStatuses":"invalid"}
]`
	transport := roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return response(http.StatusOK, raw, "application/json"), nil
	})
	client := tfl.NewClient("", time.Second, tfl.WithBaseURL("https://api.tfl.gov.uk"), tfl.WithHTTPClient(&http.Client{Transport: transport}), tfl.WithSleep(func(context.Context, time.Duration) error { return nil }))
	if err := client.SetSeverities([]tfl.Severity{{ModeName: "tube", SeverityLevel: 5, Description: "minor"}, {ModeName: "tube", SeverityLevel: 10, Description: "good"}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Partial || snapshot.Lines["central"].Good || len(snapshot.Lines["central"].Statuses) != 1 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	selected := tfl.ForSelection(snapshot, []string{"central", "victoria"}, []tfl.Line{{ID: "victoria", DisplayName: "Victoria"}})
	if !selected["victoria"].Unknown || selected["victoria"].DisplayName != "Victoria" {
		t.Fatalf("missing selected line was not unknown: %+v", selected)
	}
}

func TestSeverityValidation(t *testing.T) {
	if _, err := tfl.ValidateSeverities([]tfl.Severity{{ModeName: "tube", SeverityLevel: 1, Description: "bad"}}); err == nil {
		t.Fatal("missing severity 10 accepted")
	}
}

func TestCatalogIncludesSupportedModes(t *testing.T) {
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `[
{"id":"central","name":"Central","modeName":"tube"},
{"id":"elizabeth","name":"Elizabeth line","modeName":" Elizabeth-Line "},
{"id":"dlr","name":"DLR","modeName":"dlr"},
{"id":"windrush","name":"Windrush","modeName":"overground"},
{"id":"lioness","name":"Lioness","modeName":"overground"},
{"id":"missing","name":"Missing"},
{"id":"bus","name":"Bus","modeName":"bus"},
{"id":"combined","name":"Combined","modeName":"tube,dlr"}
]`, "application/json"), nil
	})
	client := tfl.NewClient("", time.Second, tfl.WithHTTPClient(&http.Client{Transport: transport}))
	lines, err := client.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		ids = append(ids, line.ID)
		if line.ID == "elizabeth" && line.ModeName != "elizabeth-line" {
			t.Fatalf("mode not normalized: %+v", line)
		}
	}
	if !slices.Equal(ids, []string{"central", "dlr", "elizabeth", "lioness", "windrush"}) {
		t.Fatalf("catalog IDs: %v", ids)
	}
}

func TestStatusUsesEachTransportMode(t *testing.T) {
	for _, mode := range []string{"dlr", "elizabeth-line", "overground"} {
		t.Run(mode, func(t *testing.T) {
			var statusPath string
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/Line/Meta/Severity" {
					return response(http.StatusServiceUnavailable, "", ""), nil
				}
				statusPath = r.URL.Path
				body := fmt.Sprintf(`[
{"id":"good","modeName":%q,"lineStatuses":[{"statusSeverity":10}]},
{"id":"disrupted","modeName":%q,"lineStatuses":[{"statusSeverity":5}]},
{"id":"other-mode-severity","modeName":%q,"lineStatuses":[{"statusSeverity":6}]},
{"id":"missing-mode","lineStatuses":[{"statusSeverity":10}]}
]`, mode, mode, mode)
				return response(http.StatusOK, body, "application/json"), nil
			})
			client := tfl.NewClient("", time.Second,
				tfl.WithHTTPClient(&http.Client{Transport: transport}),
				tfl.WithSleep(func(context.Context, time.Duration) error { return nil }))
			metadata := []tfl.Severity{
				{ModeName: "tube", SeverityLevel: 10, Description: "Good Service"},
				{ModeName: "tube", SeverityLevel: 6, Description: "Severe delays"},
				{ModeName: mode, SeverityLevel: 10, Description: "Good Service"},
				{ModeName: mode, SeverityLevel: 5, Description: "Minor delays"},
			}
			if err := client.SetSeverities(metadata); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				snapshot, err := client.Status(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if statusPath != "/Line/Mode/tube,dlr,elizabeth-line,overground/Status" {
					t.Fatalf("status route: %s", statusPath)
				}
				if !snapshot.Lines["good"].Good || snapshot.Lines["disrupted"].Good || snapshot.Lines["disrupted"].Unknown {
					t.Fatalf("incorrect status for %s: %+v", mode, snapshot)
				}
				for _, id := range []string{"other-mode-severity", "missing-mode"} {
					if !snapshot.Lines[id].Unknown || snapshot.Lines[id].Good {
						t.Fatalf("invalid data marked healthy: %+v", snapshot.Lines[id])
					}
				}
				selected := tfl.ForSelection(snapshot, []string{"missing-line"}, nil)
				if !selected["missing-line"].Unknown || selected["missing-line"].Good {
					t.Fatal("missing selected line marked healthy")
				}
			}
		})
	}
}

func TestMissingNewModeMetadataPreservesTube(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/Line/Meta/Severity" {
			return response(http.StatusOK, `[{"modeName":"tube","severityLevel":10,"description":"Good Service"}]`, "application/json"), nil
		}
		return response(http.StatusOK, `[
{"id":"central","modeName":"tube","lineStatuses":[{"statusSeverity":10}]},
{"id":"elizabeth","modeName":"elizabeth-line","lineStatuses":[{"statusSeverity":10}]}
]`, "application/json"), nil
	})
	client := tfl.NewClient("", time.Second, tfl.WithHTTPClient(&http.Client{Transport: transport}))
	if _, err := client.Severities(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Lines["central"].Good || !snapshot.Lines["elizabeth"].Unknown || snapshot.Lines["elizabeth"].Good {
		t.Fatalf("incomplete metadata lost valid Tube data: %+v", snapshot)
	}
}

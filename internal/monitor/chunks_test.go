package monitor

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/motohedgehog/edi/internal/message"
	"github.com/motohedgehog/edi/internal/store"
	"github.com/motohedgehog/edi/internal/tfl"
)

func TestClassifyStatus(t *testing.T) {
	tests := []struct {
		name        string
		description string
		known       bool
		reason      string
		want        StatusIndicator
	}{
		{name: "good service", description: "Good Service", known: true, want: StatusIndicator{Icon: "✅", Label: "Good Service"}},
		{name: "unrecognized good service", description: "Good Service", want: StatusIndicator{Icon: "❓", Label: "Unknown status"}},
		{name: "severe delays", description: "Severe Delays", known: true, want: StatusIndicator{Icon: "❌", Label: "Severe Delays"}},
		{name: "unmapped numeric severity with major category", description: "Severe Delays", want: StatusIndicator{Icon: "❌", Label: "Severe Delays"}},
		{name: "suspended", description: "Suspended", known: true, want: StatusIndicator{Icon: "❌", Label: "Suspended"}},
		{name: "part suspended", description: "Part Suspended", known: true, want: StatusIndicator{Icon: "❌", Label: "Part Suspended"}},
		{name: "closed", description: "Closed", known: true, want: StatusIndicator{Icon: "❌", Label: "Closed"}},
		{name: "part closure", description: "Part Closure", known: true, want: StatusIndicator{Icon: "❌", Label: "Part Closure"}},
		{name: "minor delays", description: "Minor Delays", known: true, want: StatusIndicator{Icon: "⚠️", Label: "Minor Delays"}},
		{name: "other known disruption", description: "Service Disruption", known: true, want: StatusIndicator{Icon: "⚠️", Label: "Service Disruption"}},
		{name: "reason does not classify", description: "Service Disruption", known: true, reason: "Severe Delays at Bank", want: StatusIndicator{Icon: "⚠️", Label: "Service Disruption"}},
		{name: "unknown numeric severity with minor category", description: "Minor Delays", want: StatusIndicator{Icon: "⚠️", Label: "Minor Delays"}},
		{name: "TfL unknown wording", description: "Unknown", want: StatusIndicator{Icon: "❓", Label: "Unknown"}},
		{name: "missing category with unrecognized severity", description: "Unknown status", known: true, want: StatusIndicator{Icon: "❓", Label: "Unknown status"}},
		{name: "unavailable wording", want: StatusIndicator{Icon: "❓", Label: "Unknown status"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyStatus(tfl.NormalizedStatus{Description: test.description, Known: test.known, Reason: test.reason})
			if got != test.want {
				t.Fatalf("ClassifyStatus() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRenderAutomaticAlertStatusAndRecovery(t *testing.T) {
	observed := time.Date(2026, time.September, 23, 8, 42, 0, 0, time.FixedZone("BST", 3600))
	tests := []struct {
		name string
		line AlertLine
		want []string
	}{
		{
			name: "changed major disruption retains severity label",
			line: AlertLine{LineID: "central", DisplayName: "Central", Kind: "change", Statuses: []tfl.NormalizedStatus{{Description: "Severe Delays", Known: true, Reason: "Signal failure at Liverpool Street."}}},
			want: []string{"<b>Central</b>", "❌ <b>Severe Delays</b>", "Signal failure at Liverpool Street.", "<i>Observed 23 Sep · 08:42 BST</i>"},
		},
		{
			name: "recovery says good service restored",
			line: AlertLine{LineID: "district", DisplayName: "District", Kind: "recovery", Statuses: []tfl.NormalizedStatus{{Description: "Good Service", Known: true}}},
			want: []string{"<b>District</b>", "✅ <b>Good Service restored</b>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chunk, err := RenderAutomaticAlert(test.line, observed, "Europe/London")
			if err != nil {
				t.Fatal(err)
			}
			if chunk.Format != message.FormatHTML {
				t.Fatalf("format = %v, want HTML", chunk.Format)
			}
			for _, want := range test.want {
				if !strings.Contains(chunk.Body, want) {
					t.Errorf("message %q does not contain %q", chunk.Body, want)
				}
			}
			if got := strings.TrimSpace(chunk.Body); got != chunk.Body {
				t.Errorf("message has unexpected leading or trailing whitespace: %q", got)
			}
			if !strings.HasSuffix(chunk.Body, "</i>") {
				t.Errorf("observation timestamp is not last: %q", chunk.Body)
			}
		})
	}
}

func TestRenderStatusCheckPreservesStatusesAndEscapesHTML(t *testing.T) {
	observed := time.Date(2026, time.September, 23, 8, 42, 0, 0, time.FixedZone("BST", 3600))
	chunks := RenderStatusCheck([]AlertLine{{
		LineID:      "central",
		DisplayName: "Central & <Line>",
		Statuses: []tfl.NormalizedStatus{
			{Description: "Good Service", Known: true},
			{Description: "Minor Delays & <details>", Known: true, Reason: "Earlier faulty train at Earl’s Court <platform>."},
		},
	}}, observed, "Europe/London")
	if len(chunks) != 1 {
		t.Fatalf("RenderStatusCheck() returned %d chunks, want 1", len(chunks))
	}
	got := chunks[0].Body
	for _, want := range []string{
		"<b>Central &amp; &lt;Line&gt;</b>",
		"✅ <b>Good Service</b>",
		"⚠️ <b>Minor Delays &amp; &lt;details&gt;</b>",
		"Earlier faulty train at Earl’s Court &lt;platform&gt;.",
		"<i>Observed 23 Sep · 08:42 BST</i>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "Updated") || strings.Contains(got, "restored") {
		t.Errorf("manual check contains alert-only wording: %q", got)
	}
}

func TestChangedDisruptionUsesCurrentStatusCategory(t *testing.T) {
	previous := store.MonitorState{Good: false}
	for _, test := range []struct {
		name        string
		description string
		want        string
	}{
		{name: "major to minor", description: "Minor Delays", want: "⚠️ <b>Minor Delays</b>"},
		{name: "minor to major", description: "Severe Delays", want: "❌ <b>Severe Delays</b>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := tfl.LineStatus{
				LineID: "central", DisplayName: "Central", Good: false,
				Statuses: []tfl.NormalizedStatus{{Description: test.description, Known: true}},
			}
			kind := kindFor(current, &previous, true)
			if kind != "change" {
				t.Fatalf("kindFor() = %q, want change", kind)
			}
			chunk, err := RenderAutomaticAlert(AlertLine{LineID: current.LineID, DisplayName: current.DisplayName, Kind: kind, Statuses: current.Statuses}, time.Now(), "UTC")
			if err != nil || !strings.Contains(chunk.Body, test.want) {
				t.Fatalf("changed alert = %+v err=%v, want %q", chunk, err, test.want)
			}
			if strings.Contains(chunk.Body, "Updated") {
				t.Fatalf("automatic alert contains removed wording: %q", chunk.Body)
			}
		})
	}
}

func TestUnknownStatusAndAvailabilityMessages(t *testing.T) {
	observed := time.Date(2026, time.September, 23, 8, 42, 0, 0, time.FixedZone("BST", 3600))
	unknown, err := RenderAutomaticAlert(AlertLine{LineID: "central", DisplayName: "Central", Kind: "unknown", Statuses: []tfl.NormalizedStatus{{}}}, observed, "Europe/London")
	if err != nil || !strings.Contains(unknown.Body, "❓ <b>Unknown status</b>") {
		t.Fatalf("unknown status message = %+v err=%v", unknown, err)
	}

	outage := RenderAvailability(false, 3, observed, "Europe/London")
	if len(outage) != 1 || !strings.Contains(outage[0].Body, "⚠️ <b>TfL updates unavailable</b>") || !strings.HasSuffix(outage[0].Body, "<i>Observed 23 Sep · 08:42 BST</i>") {
		t.Fatalf("outage message = %+v", outage)
	}
	restored := RenderAvailability(true, 0, observed, "Europe/London")
	if len(restored) != 1 || !strings.Contains(restored[0].Body, "✅ <b>TfL updates restored</b>") || !strings.HasSuffix(restored[0].Body, "<i>Observed 23 Sep · 08:42 BST</i>") {
		t.Fatalf("restored message = %+v", restored)
	}
}

func TestRenderStatusCheckSplitsLongUnicodeReasons(t *testing.T) {
	observed := time.Date(2026, time.September, 23, 8, 42, 0, 0, time.FixedZone("BST", 3600))
	reason := strings.Repeat("🚇駅 & <", 1300)
	chunks := RenderStatusCheck([]AlertLine{{
		LineID:      "central",
		DisplayName: "Central",
		Statuses:    []tfl.NormalizedStatus{{Description: "Minor Delays", Known: true, Reason: reason}},
	}}, observed, "Europe/London")
	if len(chunks) < 2 {
		t.Fatalf("RenderStatusCheck() returned %d chunks for a long reason, want multiple", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.Format != message.FormatHTML {
			t.Errorf("chunk %d format = %v, want HTML", i+1, chunk.Format)
		}
		if got := len(utf16.Encode([]rune(chunk.Body))); got > MaxTelegramMessage {
			t.Errorf("chunk %d has %d UTF-16 units, limit is %d", i+1, got, MaxTelegramMessage)
		}
		if !utf8.ValidString(chunk.Body) || !strings.Contains(chunk.Body, "&lt;") {
			t.Errorf("chunk %d has invalid Unicode or unescaped HTML: %q", i+1, chunk.Body[:min(120, len(chunk.Body))])
		}
		if !strings.Contains(chunk.Body, "Part "+itoa(i+1)+"/"+itoa(len(chunks))) {
			t.Errorf("chunk %d is missing its part label: %q", i+1, chunk.Body[:min(40, len(chunk.Body))])
		}
		if !strings.HasSuffix(chunk.Body, "<i>Observed 23 Sep · 08:42 BST</i>") {
			t.Errorf("chunk %d timestamp is not last: %q", i+1, chunk.Body)
		}
		if !strings.Contains(chunk.Body, "<b>Central</b>") || !strings.Contains(chunk.Body, "⚠️ <b>Minor Delays</b>") {
			t.Errorf("chunk %d lost complete line formatting: %q", i+1, chunk.Body[:min(120, len(chunk.Body))])
		}
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

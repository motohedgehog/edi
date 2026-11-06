package monitor_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/motohedgehog/edi/internal/monitor"
	"github.com/motohedgehog/edi/internal/tfl"
)

func TestAutomaticAlertKeepsLabelsAndTimeWithinOneMessage(t *testing.T) {
	line := monitor.AlertLine{LineID: "central", DisplayName: "Central <&>", Statuses: []tfl.NormalizedStatus{
		{Severity: 5, Description: "Minor delays", Reason: strings.Repeat("🧑<&>", 3000), Known: true},
		{Severity: 9, Description: "Reduced service", Reason: "Second reason", Known: true},
	}}
	chunk, err := monitor.RenderAutomaticAlert(line, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	body := chunk.Body
	if !utf8.ValidString(body) {
		t.Fatal("automatic alert split a Unicode character")
	}
	if len(utf16.Encode([]rune(body))) > monitor.MaxTelegramMessage {
		t.Fatalf("oversized message")
	}
	for _, want := range []string{"Central &lt;&amp;&gt;", "Minor delays", "Reduced service", "Observed 01 Jan", "[description omitted]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

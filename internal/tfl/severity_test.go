package tfl_test

import (
	"testing"

	"github.com/motohedgehog/edi/internal/tfl"
)

func TestSeverityKeyIgnoresOrderDuplicatesAndText(t *testing.T) {
	a := tfl.LineStatus{Statuses: []tfl.NormalizedStatus{{Severity: 5}, {Severity: 9}}}
	b := tfl.LineStatus{Statuses: []tfl.NormalizedStatus{{Severity: 9, Reason: "changed"}, {Severity: 5}, {Severity: 5}}}
	if tfl.SeverityKey(a) != tfl.SeverityKey(b) {
		t.Fatal("order, duplicates or text changed severity")
	}
	b.Statuses = b.Statuses[:1]
	if tfl.SeverityKey(a) == tfl.SeverityKey(b) {
		t.Fatal("secondary severity ignored")
	}
	b = a
	b.Unknown = true
	if tfl.SeverityKey(a) == tfl.SeverityKey(b) {
		t.Fatal("unknown flag ignored")
	}
	b = a
	b.Good = true
	if tfl.SeverityKey(a) == tfl.SeverityKey(b) {
		t.Fatal("good flag ignored")
	}
}

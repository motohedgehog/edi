package telegram

import "testing"

func TestParseCallback(t *testing.T) {
	tests := []struct {
		name         string
		value        string
		wantSession  uint64
		wantRevision int64
		wantAction   string
		wantValue    string
		wantValid    bool
	}{
		{name: "missing fields", value: "1:2", wantValid: false},
		{name: "empty action", value: "1:2::value", wantValid: false},
		{name: "malformed session", value: "x:2:save:value", wantValid: false},
		{name: "malformed revision", value: "1:x:save:value", wantValid: false},
		{name: "optional value omitted", value: "7:8:save", wantSession: 7, wantRevision: 8, wantAction: "save", wantValid: true},
		{name: "empty optional value", value: "7:8:save:", wantSession: 7, wantRevision: 8, wantAction: "save", wantValid: true},
		{name: "colons remain in value", value: "7:8:time:09:30", wantSession: 7, wantRevision: 8, wantAction: "time", wantValue: "09:30", wantValid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, revision, action, value, valid := parseCallback(test.value)
			if session != test.wantSession || revision != test.wantRevision || action != test.wantAction || value != test.wantValue || valid != test.wantValid {
				t.Fatalf("parseCallback(%q) = (%d, %d, %q, %q, %v)", test.value, session, revision, action, value, valid)
			}
		})
	}
}

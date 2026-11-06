package app

import (
	"fmt"
	"runtime/debug"
	"testing"

	"github.com/motohedgehog/edi/internal/store"
)

func TestVersionStringMetadata(t *testing.T) {
	originalCommit := Commit
	t.Cleanup(func() { Commit = originalCommit })
	info := &debug.BuildInfo{GoVersion: "go1.27.3", Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc123"},
		{Key: "vcs.modified", Value: "true"},
		{Key: "vcs.time", Value: "2026-11-05T23:56:36Z"},
	}}
	for _, tt := range []struct {
		name                       string
		info                       *debug.BuildInfo
		commit, wantCommit, wantGo string
	}{
		{"embedded", info, "unknown", "abc123", "go1.27.3"},
		{"unavailable", nil, "unknown", "unknown", "unknown"},
		{"empty", &debug.BuildInfo{}, "unknown", "unknown", "unknown"},
		{"no VCS", &debug.BuildInfo{GoVersion: "go1.27.3"}, "unknown", "unknown", "go1.27.3"},
		{"override", info, "release-sha", "release-sha", "go1.27.3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			Commit = tt.commit
			want := fmt.Sprintf("edi %s\ncommit: %s\ngo: %s\nschema: %d", Version, tt.wantCommit, tt.wantGo, store.SchemaVersion)
			if got := formatVersion(tt.info); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
			if Commit != tt.commit {
				t.Fatal("version formatting mutated build metadata")
			}
		})
	}
}

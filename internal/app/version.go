package app

import (
	"fmt"
	"runtime/debug"

	"github.com/motohedgehog/edi/internal/store"
)

var (
	Version = "dev"
	Commit  = "unknown"
)

func VersionString() string {
	info, _ := debug.ReadBuildInfo()
	return formatVersion(info)
}

func formatVersion(info *debug.BuildInfo) string {
	commit, goVersion := Commit, "unknown"
	if info != nil {
		if info.GoVersion != "" {
			goVersion = info.GoVersion
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" && commit == "unknown" {
				commit = setting.Value
			}
		}
	}
	return fmt.Sprintf("edi %s\ncommit: %s\ngo: %s\nschema: %d", Version, commit, goVersion, store.SchemaVersion)
}

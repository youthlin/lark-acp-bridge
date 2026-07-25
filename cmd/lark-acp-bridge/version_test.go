package main

import (
	"runtime/debug"
	"testing"
)

func TestAppVersionFromBuildInfo(t *testing.T) {
	tests := []struct {
		name     string
		injected string
		info     *debug.BuildInfo
		ok       bool
		want     string
	}{
		{
			name:     "ldflags override",
			injected: "v1.2.3",
			info:     &debug.BuildInfo{},
			ok:       true,
			want:     "v1.2.3",
		},
		{
			name: "module version",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v1.2.3"},
			},
			ok:   true,
			want: "v1.2.3",
		},
		{
			name: "pseudo version prefers local vcs revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20260725135515-33b56bbe6cb5+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "33b56bbe6cb5a3b5633b5929370a9d3f6e0fbd6a"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:   true,
			want: "33b56bb-dirty",
		},
		{
			name: "local vcs revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "33b56bb123456789"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			ok:   true,
			want: "33b56bb",
		},
		{
			name: "dirty local vcs revision",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "33b56bb123456789"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:   true,
			want: "33b56bb-dirty",
		},
		{
			name: "fallback",
			ok:   false,
			want: "dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appVersionFromBuildInfo(tt.injected, tt.info, tt.ok); got != tt.want {
				t.Fatalf("appVersionFromBuildInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}

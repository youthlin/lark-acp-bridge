package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestValidateTopLevelCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no command"},
		{name: "version flag already handled", args: nil},
		{name: "run mode", args: []string{"run"}},
		{name: "service command", args: []string{"service", "install"}},
		{name: "bots shorthand", args: []string{"list"}},
		{name: "bare version is unknown", args: []string{"version"}, wantErr: "无法识别的命令: lark-acp-bridge version"},
		{name: "unknown command is unknown", args: []string{"status"}, wantErr: "无法识别的命令: lark-acp-bridge status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTopLevelCommand(tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTopLevelCommand(%v) error = %v", tt.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateTopLevelCommand(%v) error = nil, want %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateTopLevelCommand(%v) error = %q, want contains %q", tt.args, err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "bots <list|add|register|create-lark-cli-profile|remove>") {
				t.Fatalf("validateTopLevelCommand(%v) error missing usage, got %q", tt.args, err)
			}
		})
	}
}

func TestTopLevelUsage(t *testing.T) {
	usage := topLevelUsage()
	wantParts := []string{
		"lark-acp-bridge [--config <path>] [--version] [run|start|stop|restart]",
		"bots <list|add|register|create-lark-cli-profile|remove>",
		"service <install|uninstall>",
		"update [--check] [--version <tag>]",
		"运行模式:",
		"子命令:",
		"-config string",
		"-version",
	}
	for _, part := range wantParts {
		if !strings.Contains(usage, part) {
			t.Fatalf("topLevelUsage() missing %q in:\n%s", part, usage)
		}
	}
	if strings.Contains(usage, "daemon-child") {
		t.Fatalf("topLevelUsage() exposes internal daemon flag:\n%s", usage)
	}
}

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

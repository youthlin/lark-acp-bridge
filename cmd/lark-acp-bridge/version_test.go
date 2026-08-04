package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func TestValidateTopLevelArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no command"},
		{name: "version flag already handled", args: nil},
		{name: "run mode", args: []string{"run"}},
		{name: "service command", args: []string{"service", "install"}},
		{name: "bots shorthand is unknown", args: []string{"list"}, wantErr: "无法识别的命令: lark-acp-bridge list"},
		{name: "bare version is unknown", args: []string{"version"}, wantErr: "无法识别的命令: lark-acp-bridge version"},
		{name: "unknown command is unknown", args: []string{"status"}, wantErr: "无法识别的命令: lark-acp-bridge status"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTopLevelArgs(tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTopLevelArgs(%v) error = %v", tt.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateTopLevelArgs(%v) error = nil, want %q", tt.args, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateTopLevelArgs(%v) error = %q, want contains %q", tt.args, err, tt.wantErr)
			}
		})
	}
}

func TestTopLevelUsage(t *testing.T) {
	usage, err := commandOutput("--help")
	if err != nil {
		t.Fatalf("commandOutput(--help) error = %v", err)
	}
	wantParts := []string{
		"Usage:",
		"Available Commands:",
		"bots",
		"service",
		"update",
		"run",
		"start",
		"stop",
		"restart",
		"--config string",
		"--version",
	}
	for _, part := range wantParts {
		if !strings.Contains(usage, part) {
			t.Fatalf("--help missing %q in:\n%s", part, usage)
		}
	}
	if strings.Contains(usage, "daemon-child") {
		t.Fatalf("--help exposes internal daemon flag:\n%s", usage)
	}
	if strings.Contains(usage, "completion") {
		t.Fatalf("--help exposes completion command:\n%s", usage)
	}
}

func TestTopLevelVersionFlag(t *testing.T) {
	oldVersion := version
	version = "v9.9.9"
	t.Cleanup(func() {
		version = oldVersion
	})
	out, err := commandOutput("--version")
	if err != nil {
		t.Fatalf("commandOutput(--version) error = %v", err)
	}
	if strings.TrimSpace(out) != "v9.9.9" {
		t.Fatalf("--version output = %q, want v9.9.9", out)
	}

	out, err = commandOutput("-version")
	if err == nil || !strings.Contains(err.Error(), "unknown shorthand flag: 'v'") {
		t.Fatalf("commandOutput(-version) error = %v, output = %q, want shorthand flag error", err, out)
	}
}

func TestRemovedCompatibilityPaths(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "single dash long subcommand flag",
			args:    []string{"bots", "register", "-timeout", "2m", "default"},
			wantErr: "unknown shorthand flag: 't'",
		},
		{
			name:    "service remove alias",
			args:    []string{"service", "remove"},
			wantErr: "用法: lark-acp-bridge service <install|uninstall>",
		},
		{
			name:    "service rm alias",
			args:    []string{"service", "rm"},
			wantErr: "用法: lark-acp-bridge service <install|uninstall>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := commandOutput(tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("commandOutput(%v) error = %v, output = %q, want contains %q", tt.args, err, out, tt.wantErr)
			}
		})
	}
}

func commandOutput(args ...string) (string, error) {
	cmd := newRootCommand()
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
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

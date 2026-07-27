package bridge

import (
	"strings"
	"testing"
	"time"
)

func TestParseLoopRequest(t *testing.T) {
	req, err := parseLoopRequest("/loop -t 30m -n 3 -i 2s 持续推进")
	if err != nil {
		t.Fatalf("parseLoopRequest() error = %v", err)
	}
	if req.MaxDuration != 30*time.Minute || req.MaxRounds != 3 || req.Interval != 2*time.Second || req.Prompt != "持续推进" {
		t.Fatalf("request = %+v, want parsed loop options", req)
	}

	req, err = parseLoopRequest("/loop -t 0 -n 0 默认间隔")
	if err != nil {
		t.Fatalf("parseLoopRequest(unlimited) error = %v", err)
	}
	if req.MaxDuration != 0 || req.MaxRounds != 0 || req.Interval != defaultLoopInterval || req.Prompt != "默认间隔" {
		t.Fatalf("request = %+v, want unlimited with default interval", req)
	}

	req, err = parseLoopRequest("/loop -n 1 请保留  多空格\n以及换行")
	if err != nil {
		t.Fatalf("parseLoopRequest(spaced prompt) error = %v", err)
	}
	if req.Prompt != "请保留  多空格\n以及换行" {
		t.Fatalf("prompt = %q, want original spacing", req.Prompt)
	}

	if _, err := parseLoopRequest("/loop -i 0 提示词"); err == nil || !strings.Contains(err.Error(), "-i 必须大于 0") {
		t.Fatalf("parseLoopRequest(-i 0) error = %v, want interval validation", err)
	}
	if _, err := parseLoopRequest("/loop -x 提示词"); err == nil || !strings.Contains(err.Error(), "未知 loop 参数") {
		t.Fatalf("parseLoopRequest(-x) error = %v, want unknown option", err)
	}
	if _, err := parseLoopRequest("/loop -n 1"); err == nil || !strings.Contains(err.Error(), "提示词必填") {
		t.Fatalf("parseLoopRequest(no prompt) error = %v, want required prompt", err)
	}
}

func TestLoopDone(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "plain", text: "DONE", want: true},
		{name: "space", text: " \nDONE\t", want: true},
		{name: "inline code not accepted", text: "`DONE`"},
		{name: "plain fenced not accepted", text: "```\nDONE\n```"},
		{name: "typed fenced not accepted", text: "```text\nDONE\n```"},
		{name: "lowercase not accepted", text: "done"},
		{name: "extra text not accepted", text: "DONE\n继续"},
		{name: "sentence not accepted", text: "DONE，已完成"},
		{name: "typed fenced extra text not accepted", text: "```text\nDONE\n继续\n```"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := loopDone(tt.text); got != tt.want {
				t.Fatalf("loopDone(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

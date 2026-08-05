package bridge

import "testing"

func TestIsSilentReplySentinel(t *testing.T) {
	compactedNotice := "Context compacted Heads up: Long threads and multiple compactions can cause the model to be less accurate. Start a new thread when possible to keep threads small and targeted."
	tests := []struct {
		name  string
		reply string
		want  bool
	}{
		{
			name:  "exact",
			reply: "SILENT",
			want:  true,
		},
		{
			name:  "exact ignores case and whitespace",
			reply: "\n silent \t",
			want:  true,
		},
		{
			name:  "context compacted notice before sentinel",
			reply: compactedNotice + "SILENT",
			want:  true,
		},
		{
			name:  "context compacted notice with newline before sentinel",
			reply: compactedNotice + "\nSILENT\n",
			want:  true,
		},
		{
			name:  "other agent notice before sentinel",
			reply: "Conversation state was summarized by the ACP agent.\nSILENT",
			want:  true,
		},
		{
			name:  "sentence ending with standalone sentinel",
			reply: "Document that unrelated auto-judgement replies should output SILENT",
			want:  true,
		},
		{
			name:  "punctuation before sentinel",
			reply: "No response is needed, so SILENT",
			want:  true,
		},
		{
			name:  "word suffix is not sentinel",
			reply: compactedNotice + "NOTSILENT",
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSilentReplySentinel(tt.reply); got != tt.want {
				t.Fatalf("isSilentReplySentinel(%q) = %v, want %v", tt.reply, got, tt.want)
			}
		})
	}
}

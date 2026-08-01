package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestPromptCardStreamCreatesCardOnceConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	var cards []*fakeStreamCard
	ctx := context.Background()
	starter := streamCardStarterFunc(func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		started <- struct{}{}
		<-release
		card := &fakeStreamCard{}
		mu.Lock()
		cards = append(cards, card)
		mu.Unlock()
		return card, nil
	})
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{}, starter)

	done := make(chan struct{}, 2)
	go func() {
		stream.updateText("hello")
		done <- struct{}{}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream card starter was not called")
	}
	go func() {
		stream.updateProcess("process")
		done <- struct{}{}
	}()
	select {
	case <-started:
		t.Fatal("stream card starter was called twice while first creation was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent stream update did not finish")
		}
	}
	mu.Lock()
	gotCards := len(cards)
	if gotCards != 1 {
		mu.Unlock()
		t.Fatalf("cards = %d, want one stream card", gotCards)
	}
	card := cards[0]
	mu.Unlock()
	if got := card.textUpdatesSnapshot(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("textUpdates = %+v, want text update on single card", got)
	}
	if got := card.processUpdatesSnapshot(); len(got) != 1 || got[0] != "process" {
		t.Fatalf("processUpdates = %+v, want process update on single card", got)
	}
}

func TestPromptCardStreamTruncatesLongProcessText(t *testing.T) {
	var cards []*fakeStreamCard
	ctx := context.Background()
	starter := streamCardStarterFunc(func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{}, starter)

	stream.updateProcess(strings.Repeat("前", maxPromptProcessRunes+20) + "尾部")

	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	got := cards[0].processUpdatesSnapshot()
	if len(got) != 1 {
		t.Fatalf("processUpdates = %+v, want one process update", got)
	}
	if !strings.HasPrefix(got[0], "（前面过程内容已省略）\n") {
		t.Fatalf("process update prefix = %q, want omission marker", got[0])
	}
	if !strings.HasSuffix(got[0], "尾部") {
		t.Fatalf("process update suffix = %q, want tail retained", got[0])
	}
	if len([]rune(got[0])) > maxPromptProcessRunes+20 {
		t.Fatalf("process update length = %d, want bounded text", len([]rune(got[0])))
	}
}

func TestPromptCardStreamThrottlesProcessUpdatesUntilClose(t *testing.T) {
	var cards []*fakeStreamCard
	ctx := context.Background()
	starter := streamCardStarterFunc(func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cards = append(cards, card)
		return card, nil
	})
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{}, starter)

	stream.updateProcess("one")
	stream.updateProcess("two")

	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].processUpdatesSnapshot(); len(got) != 1 || got[0] != "one" {
		t.Fatalf("processUpdates = %+v, want second process update throttled", got)
	}
	stream.close()
	if got := cards[0].processUpdatesSnapshot(); len(got) != 2 || got[1] != "one\ntwo" {
		t.Fatalf("processUpdates = %+v, want pending process flushed on close", got)
	}
}

func TestProcessPanelTextKeepsProcessRowsCompact(t *testing.T) {
	got := processPanelText([]string{
		"📌 计划\n• ✅ 读取现有实现\n• 🔄 修复展示",
		"✅ go test ./...",
		"💬 继续处理",
	})
	want := "📌 计划\n• ✅ 读取现有实现\n• 🔄 修复展示\n✅ go test ./...\n💬 继续处理"
	if got != want {
		t.Fatalf("processPanelText() = %q, want %q", got, want)
	}
}

func TestPromptChunkAccumulatorDebouncesShortTextChunks(t *testing.T) {
	var cardsMu sync.Mutex
	var cards []*fakeStreamCard
	ctx := context.Background()
	starter := streamCardStarterFunc(func(ctx context.Context, msg feishu.Message) (feishu.StreamCard, error) {
		card := &fakeStreamCard{}
		cardsMu.Lock()
		cards = append(cards, card)
		cardsMu.Unlock()
		return card, nil
	})
	cardsSnapshot := func() []*fakeStreamCard {
		cardsMu.Lock()
		defer cardsMu.Unlock()
		return append([]*fakeStreamCard(nil), cards...)
	}
	stream := newPromptCardStream(ctx, feishu.Message{
		MessageID: "om_msg",
		ChatID:    "oc_private",
		ChatType:  "p2p",
	}, Session{ACPSessionID: "acp-session-1"}, ChatConfig{}, starter)
	chunks := newPromptChunkAccumulator(stream)
	chunks.add(promptChunk{Target: promptChunkTargetText, Key: "agent_message", Text: "Hel"})
	chunks.add(promptChunk{Target: promptChunkTargetText, Key: "agent_message", Text: "lo"})
	if got := cardsSnapshot(); len(got) != 0 {
		t.Fatalf("cards = %+v, want debounce to delay card creation", got)
	}
	time.Sleep(promptCardFlushDelay + 80*time.Millisecond)
	gotCards := cardsSnapshot()
	if len(gotCards) != 1 {
		t.Fatalf("cards = %+v, want one stream card after debounce flush", gotCards)
	}
	if got := gotCards[0].textUpdatesSnapshot(); len(got) != 1 || got[0] != "Hello" {
		t.Fatalf("textUpdates = %+v, want one debounced update", got)
	}
	chunks.close()
}

func TestNormalizeStreamMarkdownFoldsSoftLineBreaks(t *testing.T) {
	input := strings.Join([]string{
		"hello",
		"world",
		"from",
		"ACP.",
		"下一句",
		"继续",
		"",
		"- item 1",
		"- item 2",
		"",
		"```",
		"line 1",
		"line 2",
		"```",
	}, "\n")
	want := strings.Join([]string{
		"hello world from ACP.",
		"下一句继续",
		"",
		"- item 1",
		"- item 2",
		"",
		"```",
		"line 1",
		"line 2",
		"```",
	}, "\n")
	if got := normalizeStreamMarkdown(input); got != want {
		t.Fatalf("normalizeStreamMarkdown() = %q, want %q", got, want)
	}
}

func TestNormalizeStreamMarkdownKeepsProcessMarkersOnSeparateLines(t *testing.T) {
	input := strings.Join([]string{
		"📌 计划",
		"• ✅ 读取现有实现",
		"• 🔄 补过程消息展示",
		"✅ go test ./...",
		"💬 继续处理",
	}, "\n")
	want := strings.Join([]string{
		"📌 计划",
		"• ✅ 读取现有实现",
		"• 🔄 补过程消息展示",
		"✅ go test ./...",
		"💬 继续处理",
	}, "\n")
	if got := normalizeStreamMarkdown(input); got != want {
		t.Fatalf("normalizeStreamMarkdown() = %q, want %q", got, want)
	}
}

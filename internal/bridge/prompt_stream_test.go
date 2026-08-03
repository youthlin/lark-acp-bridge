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

func TestPromptCardStreamRefreshesStatusWhenProcessUpdates(t *testing.T) {
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

	stream.updateProcess("tool started")

	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want one stream card", cards)
	}
	if got := cards[0].processUpdatesSnapshot(); len(got) != 1 || got[0] != "tool started" {
		t.Fatalf("processUpdates = %+v, want process update", got)
	}
	status := cards[0].statusUpdatesSnapshot()
	if len(status) != 1 {
		t.Fatalf("statusUpdates = %+v, want status refreshed with process update", status)
	}
	if !strings.HasPrefix(status[0], "⏳ ") {
		t.Fatalf("statusUpdates[0] = %q, want running elapsed status", status[0])
	}
}

func TestPromptProcessUpdateThrottlerBoundaries(t *testing.T) {
	now := time.Unix(100, 0)
	throttler := promptProcessUpdateThrottler{interval: time.Second}
	var timerGenerations []int64

	if text, ok := throttler.queueLocked(now, "one", false, func(generation int64) {
		timerGenerations = append(timerGenerations, generation)
	}); !ok || text != "one" {
		t.Fatalf("first queue = %q, %v, want immediate one", text, ok)
	}
	if throttler.lastFlush != now {
		t.Fatalf("lastFlush = %v, want %v", throttler.lastFlush, now)
	}

	if text, ok := throttler.queueLocked(now.Add(100*time.Millisecond), "two", false, func(generation int64) {
		timerGenerations = append(timerGenerations, generation)
	}); ok || text != "" {
		t.Fatalf("second queue = %q, %v, want throttled", text, ok)
	}
	if throttler.timer == nil {
		t.Fatal("timer = nil, want delayed flush timer")
	}
	if text, ok := throttler.takeLocked(now.Add(200 * time.Millisecond)); !ok || text != "two" {
		t.Fatalf("takeLocked = %q, %v, want pending two", text, ok)
	}
	if text, ok := throttler.takeLocked(now.Add(300 * time.Millisecond)); ok || text != "" {
		t.Fatalf("takeLocked after clean = %q, %v, want empty", text, ok)
	}
	throttler.stopTimerLocked()
	throttler.wait()
	if len(timerGenerations) != 0 {
		t.Fatalf("timerGenerations = %+v, want stopped timer not fired", timerGenerations)
	}

	if text, ok := throttler.queueLocked(now.Add(400*time.Millisecond), "three", true, nil); !ok || text != "three" {
		t.Fatalf("forced queue = %q, %v, want immediate three", text, ok)
	}
	if throttler.timer != nil {
		t.Fatal("timer should be stopped after forced flush")
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

func TestToolProgressCarriesTitleFromStartUpdate(t *testing.T) {
	s := &promptCardStream{showTools: true}

	// 起始事件带 title 和 toolCallId,显示为运行中。
	s.applyToolProgressLineLocked(toolProgressRunning, "call-1", "read: foo.go")
	// 更新/结束事件只带 toolCallId,没有 title(omp 的 tool_call_update 即如此)。
	s.applyToolProgressLineLocked(toolProgressCompleted, "call-1", "")

	if len(s.process) != 1 {
		t.Fatalf("process lines = %d, want 1 (start/end must share one row): %v", len(s.process), s.process)
	}
	want := "✅ read: foo.go"
	if s.process[0] != want {
		t.Fatalf("process line = %q, want %q", s.process[0], want)
	}
	for i, row := range s.tools {
		if row.active {
			t.Fatalf("tool row %d still active after completion: %+v", i, row)
		}
	}
}

func TestToolProgressWithoutIdFallsBackToTitle(t *testing.T) {
	s := &promptCardStream{showTools: true}
	s.applyToolProgressLineLocked(toolProgressRunning, "", "bash")
	s.applyToolProgressLineLocked(toolProgressCompleted, "", "bash")
	if len(s.process) != 1 {
		t.Fatalf("process lines = %d, want 1: %v", len(s.process), s.process)
	}
	if !strings.Contains(s.process[0], "bash") {
		t.Fatalf("process line = %q, want to contain tool title", s.process[0])
	}
}

func TestToolProgressWithoutTitleUsesPlaceholderOnce(t *testing.T) {
	s := &promptCardStream{showTools: true}
	s.applyToolProgressLineLocked(toolProgressRunning, "call-x", "")
	s.applyToolProgressLineLocked(toolProgressCompleted, "call-x", "")
	if len(s.process) != 1 {
		t.Fatalf("process lines = %d, want 1: %v", len(s.process), s.process)
	}
	if !strings.Contains(s.process[0], "工具调用") {
		t.Fatalf("process line = %q, want placeholder 工具调用", s.process[0])
	}
}

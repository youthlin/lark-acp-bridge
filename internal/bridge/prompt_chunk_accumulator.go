package bridge

import (
	"strings"
	"sync"
	"time"
)

const (
	promptChunkFlushRunes = 300

	promptChunkTargetText           = "text"
	promptChunkTargetProcess        = "process"
	promptChunkTargetProcessMessage = "process_message"
	promptChunkTargetThought        = "thought"
	promptChunkTargetPlan           = "plan"
	promptChunkTargetTool           = "tool"
)

type promptChunk struct {
	Target       string
	Key          string
	Text         string
	ToolBoundary bool
}

type promptChunkStream struct {
	target  string
	key     string
	pending strings.Builder
	full    strings.Builder
}

type promptChunkFlush struct {
	target string
	text   string
	finish bool
}

type promptChunkAccumulator struct {
	mu              sync.Mutex
	stream          *promptCardStream
	current         *promptChunkStream
	reply           strings.Builder
	finalCandidate  strings.Builder
	hasTool         bool
	timer           *time.Timer
	timerGeneration int64
	flushing        sync.WaitGroup
}

func newPromptChunkAccumulator(stream *promptCardStream) *promptChunkAccumulator {
	return &promptChunkAccumulator{stream: stream}
}

func (a *promptChunkAccumulator) add(chunk promptChunk) {
	if chunk.Text == "" {
		return
	}
	var flushes []promptChunkFlush
	a.mu.Lock()
	if a.current == nil || a.current.target != chunk.Target || a.current.key != chunk.Key {
		flushes = append(flushes, a.takeFlushLocked(true))
		a.current = &promptChunkStream{target: chunk.Target, key: chunk.Key}
	}
	current := a.current
	current.pending.WriteString(chunk.Text)
	current.full.WriteString(chunk.Text)
	shouldFlush := strings.Contains(chunk.Text, "\n") || len([]rune(current.pending.String())) >= promptChunkFlushRunes
	if chunk.Target == promptChunkTargetText {
		a.reply.WriteString(chunk.Text)
		a.finalCandidate.WriteString(chunk.Text)
	}
	if shouldFlush {
		flushes = append(flushes, a.takeFlushLocked(false))
		a.stopTimerLocked()
	} else {
		a.scheduleLocked()
	}
	a.mu.Unlock()
	for _, flush := range flushes {
		a.applyFlush(flush)
	}
}

func (a *promptChunkAccumulator) markToolBoundary() {
	var flushes []promptChunkFlush
	a.mu.Lock()
	flushes = append(flushes, a.takeFlushLocked(true))
	a.stopTimerLocked()
	processText := strings.TrimSpace(a.finalCandidate.String())
	if processText != "" {
		flushes = append(flushes, promptChunkFlush{target: promptChunkTargetProcessMessage, text: processText, finish: true})
		a.finalCandidate.Reset()
	}
	a.hasTool = true
	a.mu.Unlock()
	for _, flush := range flushes {
		a.applyFlush(flush)
	}
}

func (a *promptChunkAccumulator) hasToolBoundary() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasTool
}

func (a *promptChunkAccumulator) flush() {
	a.mu.Lock()
	flush := a.takeFlushLocked(false)
	a.mu.Unlock()
	a.applyFlush(flush)
}

func (a *promptChunkAccumulator) finishStream() {
	a.mu.Lock()
	a.stopTimerLocked()
	flush := a.takeFlushLocked(true)
	a.mu.Unlock()
	a.applyFlush(flush)
}

func (a *promptChunkAccumulator) close() {
	a.finishStream()
	a.flushing.Wait()
}

func (a *promptChunkAccumulator) replyText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.reply.String())
}

func (a *promptChunkAccumulator) finalText() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if text := strings.TrimSpace(a.finalCandidate.String()); text != "" {
		return text
	}
	return strings.TrimSpace(a.reply.String())
}

func (a *promptChunkAccumulator) takeFlushLocked(finish bool) promptChunkFlush {
	if a.current == nil {
		return promptChunkFlush{}
	}
	current := a.current
	hasPending := current.pending.Len() > 0
	text := strings.TrimSpace(current.full.String())
	if current.target == promptChunkTargetText {
		text = strings.TrimSpace(a.finalCandidate.String())
	}
	if finish {
		a.current = nil
	} else {
		current.pending.Reset()
	}
	if !hasPending {
		return promptChunkFlush{target: current.target, finish: finish}
	}
	return promptChunkFlush{target: current.target, text: text, finish: finish}
}

func (a *promptChunkAccumulator) applyFlush(flush promptChunkFlush) {
	if flush.target == "" {
		return
	}
	switch flush.target {
	case promptChunkTargetText:
		if flush.text != "" {
			a.stream.updateText(flush.text)
		}
	case promptChunkTargetProcessMessage:
		if flush.text != "" {
			a.stream.updateProcessMessage(flush.text)
		}
	case promptChunkTargetThought:
		if flush.text != "" {
			a.stream.updateThoughtStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	case promptChunkTargetPlan:
		if flush.text != "" {
			a.stream.updatePlanStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	case promptChunkTargetTool:
		if flush.text != "" {
			a.stream.updateToolStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	case promptChunkTargetProcess:
		if flush.text != "" {
			a.stream.updateProcessStream(flush.text)
		}
		if flush.finish {
			a.stream.finishProcessStream()
		}
	}
}

func (a *promptChunkAccumulator) scheduleLocked() {
	a.timerGeneration++
	generation := a.timerGeneration
	if a.timer != nil {
		if a.timer.Stop() {
			a.flushing.Done()
		}
	}
	a.flushing.Add(1)
	a.timer = time.AfterFunc(promptCardFlushDelay, func() {
		defer a.flushing.Done()
		a.mu.Lock()
		if a.timerGeneration != generation {
			a.mu.Unlock()
			return
		}
		flush := a.takeFlushLocked(false)
		a.timer = nil
		a.mu.Unlock()
		a.applyFlush(flush)
	})
}

func (a *promptChunkAccumulator) stopTimerLocked() {
	a.timerGeneration++
	if a.timer == nil {
		return
	}
	if a.timer.Stop() {
		a.flushing.Done()
	}
	a.timer = nil
}

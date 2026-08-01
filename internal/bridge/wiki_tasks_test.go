package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/config"
)

func TestWikiTimerRunsSilentReflection(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	rt := &fakeRuntime{promptReply: "NoReply"}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	key := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_chat", SubID: "omt_thread"})
	session := Session{
		Key:             key,
		AgentName:       "traex",
		ACPSessionID:    "acp-session-1",
		Cwd:             t.TempDir(),
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		WikiIntervalSec: 1,
	}
	svc.scheduleWikiAfterUserPrompt(session, mustConfigAgent(t, config.Default(), "traex"))

	waitForCondition(t, 2*time.Second, func() bool { return rt.promptCallCount() == 1 })
	if got := rt.promptCalls[0].Text; !strings.Contains(got, "请对刚才的对话进行反思") || !strings.Contains(got, "NoReply") {
		t.Fatalf("wiki prompt = %q, want reflection prompt", got)
	}
	svc.taskMu.Lock()
	status := svc.wikiStatuses[key]
	_, hasTimer := svc.wikiTimers[key]
	svc.taskMu.Unlock()
	if hasTimer {
		t.Fatal("wiki timer should not reschedule itself after reflection")
	}
	if !status.lastSuccess || status.running {
		t.Fatalf("wiki status = %+v, want completed success", status)
	}
}

func TestWikiStatusSnapshotSessionWorkBoundaries(t *testing.T) {
	agent := config.AgentConfig{Command: "traex"}
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	started := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name               string
		setup              func(t *testing.T, svc *Service)
		wantTimerSet       bool
		wantForegroundKind taskKind
		wantBackgroundTask bool
		wantLastSuccess    bool
		wantLastError      string
	}{
		{
			name: "读取等待触发的 wiki timer",
			setup: func(t *testing.T, svc *Service) {
				timer := time.NewTimer(time.Hour)
				t.Cleanup(func() { timer.Stop() })
				svc.taskMu.Lock()
				svc.wikiTimers[normalizedKey] = &pendingWikiRun{timer: timer, session: Session{Key: normalizedKey}}
				svc.taskMu.Unlock()
			},
			wantTimerSet: true,
		},
		{
			name: "读取前台 wiki task",
			setup: func(t *testing.T, svc *Service) {
				_, finish := svc.startTask(context.Background(), Session{Key: normalizedKey, AgentName: "traex"}, agent, taskKindWiki)
				t.Cleanup(finish)
			},
			wantForegroundKind: taskKindWiki,
		},
		{
			name: "读取后台 wiki runtime",
			setup: func(t *testing.T, svc *Service) {
				_, finish := svc.startWikiTask(context.Background(), Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-wiki"}, agent, wikiRuntimeKey(normalizedKey, 1, "acp-wiki"))
				t.Cleanup(finish)
			},
			wantBackgroundTask: true,
		},
		{
			name: "读取最近一次 wiki 状态",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiStatuses[normalizedKey] = wikiRunStatus{
					lastStarted: started,
					lastEnded:   started.Add(time.Second),
					lastSuccess: false,
					lastError:   "失败原因",
				}
				svc.taskMu.Unlock()
			},
			wantLastSuccess: false,
			wantLastError:   "失败原因",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			tt.setup(t, svc)

			snapshot := svc.wikiStatusSnapshot(key)
			if snapshot.timerSet != tt.wantTimerSet {
				t.Fatalf("timerSet = %v, want %v", snapshot.timerSet, tt.wantTimerSet)
			}
			if tt.wantForegroundKind != "" {
				if snapshot.foregroundTask == nil || snapshot.foregroundTask.kind != tt.wantForegroundKind {
					t.Fatalf("foregroundTask = %+v, want kind %s", snapshot.foregroundTask, tt.wantForegroundKind)
				}
			} else if snapshot.foregroundTask != nil {
				t.Fatalf("foregroundTask = %+v, want nil", snapshot.foregroundTask)
			}
			if snapshot.backgroundTask != tt.wantBackgroundTask {
				t.Fatalf("backgroundTask = %v, want %v", snapshot.backgroundTask, tt.wantBackgroundTask)
			}
			if tt.wantLastError != "" {
				if snapshot.status.lastSuccess != tt.wantLastSuccess || snapshot.status.lastError != tt.wantLastError || !snapshot.status.lastStarted.Equal(started) {
					t.Fatalf("status = %+v, want last wiki status", snapshot.status)
				}
			}
		})
	}
}

func TestWikiTimerSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	newServiceWithTimers := func(t *testing.T) (*Service, *time.Timer, *time.Timer) {
		t.Helper()
		svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
		timer := time.NewTimer(time.Hour)
		otherTimer := time.NewTimer(time.Hour)
		t.Cleanup(func() {
			timer.Stop()
			otherTimer.Stop()
		})
		svc.taskMu.Lock()
		svc.wikiGenerations[normalizedKey] = 10
		svc.wikiGenerations[otherKey] = 20
		svc.wikiTimers[normalizedKey] = &pendingWikiRun{
			timer:      timer,
			generation: 10,
			session:    Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-a"},
			agent:      config.AgentConfig{Command: "traex"},
		}
		svc.wikiTimers[otherKey] = &pendingWikiRun{
			timer:      otherTimer,
			generation: 20,
			session:    Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-b"},
			agent:      config.AgentConfig{Command: "traex"},
		}
		svc.taskMu.Unlock()
		return svc, timer, otherTimer
	}
	cases := []struct {
		name         string
		run          func(t *testing.T, svc *Service)
		wantHasKey   bool
		wantOther    bool
		wantKeyGen   int64
		wantOtherGen int64
	}{
		{
			name: "hasWikiTimer 规范化 key 后只读取不修改",
			run: func(t *testing.T, svc *Service) {
				if !svc.hasWikiTimer(key) {
					t.Fatal("hasWikiTimer = false, want true")
				}
			},
			wantHasKey:   true,
			wantOther:    true,
			wantKeyGen:   10,
			wantOtherGen: 20,
		},
		{
			name: "cancelWikiTimer 规范化 key 后移除并推进代际",
			run: func(t *testing.T, svc *Service) {
				svc.cancelWikiTimer(key)
			},
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   11,
			wantOtherGen: 20,
		},
		{
			name: "takePendingWiki 规范化 key 后取出并推进代际",
			run: func(t *testing.T, svc *Service) {
				pending, ok := svc.takePendingWiki(key)
				if !ok {
					t.Fatal("takePendingWiki ok = false, want true")
				}
				if pending.session.Key != normalizedKey {
					t.Fatalf("pending session key = %+v, want %+v", pending.session.Key, normalizedKey)
				}
			},
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   11,
			wantOtherGen: 20,
		},
		{
			name: "takePendingWiki 缺失 timer 时仍推进代际失效旧回调",
			run: func(t *testing.T, svc *Service) {
				svc.cancelWikiTimer(key)
				if _, ok := svc.takePendingWiki(key); ok {
					t.Fatal("takePendingWiki ok = true, want false")
				}
			},
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   12,
			wantOtherGen: 20,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newServiceWithTimers(t)

			tt.run(t, svc)

			svc.taskMu.Lock()
			_, hasKey := svc.wikiTimers[normalizedKey]
			_, hasOther := svc.wikiTimers[otherKey]
			keyGen := svc.wikiGenerations[normalizedKey]
			otherGen := svc.wikiGenerations[otherKey]
			svc.taskMu.Unlock()
			if hasKey != tt.wantHasKey {
				t.Fatalf("wikiTimers key exists = %v, want %v", hasKey, tt.wantHasKey)
			}
			if hasOther != tt.wantOther {
				t.Fatalf("wikiTimers other exists = %v, want %v", hasOther, tt.wantOther)
			}
			if keyGen != tt.wantKeyGen {
				t.Fatalf("wiki generation = %d, want %d", keyGen, tt.wantKeyGen)
			}
			if otherGen != tt.wantOtherGen {
				t.Fatalf("other wiki generation = %d, want %d", otherGen, tt.wantOtherGen)
			}
		})
	}
}

func TestScheduleWikiTimerSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	agent := config.AgentConfig{Command: "traex"}
	oldTimer := time.NewTimer(time.Hour)
	oldTimer.Stop()
	cases := []struct {
		name          string
		setup         func(t *testing.T, svc *Service)
		run           func(t *testing.T, svc *Service)
		wantGen       int64
		wantSessionID string
		wantScheduled bool
	}{
		{
			name: "scheduleWikiTimer 规范化 key 并替换旧 timer",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiGenerations[normalizedKey] = 3
				svc.wikiTimers[normalizedKey] = &pendingWikiRun{
					timer:      oldTimer,
					generation: 3,
					session:    Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-old"},
					agent:      agent,
					scheduled:  time.Date(2020, 8, 1, 11, 0, 0, 0, time.UTC),
				}
				svc.taskMu.Unlock()
			},
			run: func(t *testing.T, svc *Service) {
				svc.scheduleWikiTimer(key, time.Hour, pendingWikiRun{
					session:   Session{Key: key, AgentName: "traex", ACPSessionID: "acp-new"},
					agent:     agent,
					scheduled: time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
				})
			},
			wantGen:       4,
			wantSessionID: "acp-new",
			wantScheduled: true,
		},
		{
			name: "restorePendingWiki 复用 timer 登记边界",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiGenerations[normalizedKey] = 8
				svc.taskMu.Unlock()
			},
			run: func(t *testing.T, svc *Service) {
				svc.restorePendingWiki(pendingWikiRun{
					session:   Session{Key: key, AgentName: "traex", ACPSessionID: "acp-restored", WikiIntervalSec: 60},
					agent:     agent,
					scheduled: time.Now().Add(-time.Minute),
				})
			},
			wantGen:       9,
			wantSessionID: "acp-restored",
			wantScheduled: true,
		},
		{
			name: "restorePendingWiki 缺少 acp session 时不登记 timer",
			setup: func(t *testing.T, svc *Service) {
				svc.taskMu.Lock()
				svc.wikiGenerations[normalizedKey] = 12
				svc.taskMu.Unlock()
			},
			run: func(t *testing.T, svc *Service) {
				svc.restorePendingWiki(pendingWikiRun{
					session:   Session{Key: key, AgentName: "traex"},
					agent:     agent,
					scheduled: time.Now(),
				})
			},
			wantGen:       12,
			wantSessionID: "",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			tt.setup(t, svc)

			tt.run(t, svc)

			svc.taskMu.Lock()
			pending := svc.wikiTimers[normalizedKey]
			gen := svc.wikiGenerations[normalizedKey]
			svc.taskMu.Unlock()
			if gen != tt.wantGen {
				t.Fatalf("wiki generation = %d, want %d", gen, tt.wantGen)
			}
			if tt.wantSessionID == "" {
				if pending != nil {
					t.Fatalf("pending wiki = %+v, want nil", pending)
				}
				return
			}
			if pending == nil {
				t.Fatal("pending wiki = nil, want scheduled timer")
			}
			t.Cleanup(func() {
				if pending.timer != nil {
					pending.timer.Stop()
				}
			})
			if pending.session.Key != normalizedKey {
				t.Fatalf("pending session key = %+v, want %+v", pending.session.Key, normalizedKey)
			}
			if pending.session.ACPSessionID != tt.wantSessionID {
				t.Fatalf("pending acp session = %q, want %q", pending.session.ACPSessionID, tt.wantSessionID)
			}
			if pending.generation != tt.wantGen {
				t.Fatalf("pending generation = %d, want %d", pending.generation, tt.wantGen)
			}
			if tt.wantScheduled && pending.scheduled.IsZero() {
				t.Fatal("pending scheduled is zero, want original schedule time")
			}
		})
	}
}

func TestBeginWikiTimerRunSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	cases := []struct {
		name         string
		generation   int64
		busy         bool
		wantState    wikiTimerRunState
		wantHasKey   bool
		wantOther    bool
		wantKeyGen   int64
		wantOtherGen int64
	}{
		{
			name:         "旧代际回调不清理当前 timer",
			generation:   9,
			wantState:    wikiTimerRunStale,
			wantHasKey:   true,
			wantOther:    true,
			wantKeyGen:   10,
			wantOtherGen: 20,
		},
		{
			name:         "当前代际空闲时取走 timer 并允许执行",
			generation:   10,
			wantState:    wikiTimerRunReady,
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   10,
			wantOtherGen: 20,
		},
		{
			name:         "当前代际忙碌时取走 timer 并推进代际等待重排",
			generation:   10,
			busy:         true,
			wantState:    wikiTimerRunBusy,
			wantHasKey:   false,
			wantOther:    true,
			wantKeyGen:   11,
			wantOtherGen: 20,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			timer := time.NewTimer(time.Hour)
			otherTimer := time.NewTimer(time.Hour)
			t.Cleanup(func() {
				timer.Stop()
				otherTimer.Stop()
			})
			svc.taskMu.Lock()
			svc.wikiGenerations[normalizedKey] = 10
			svc.wikiGenerations[otherKey] = 20
			svc.wikiTimers[normalizedKey] = &pendingWikiRun{
				timer:      timer,
				generation: 10,
				session:    Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-a"},
				agent:      config.AgentConfig{Command: "traex"},
			}
			svc.wikiTimers[otherKey] = &pendingWikiRun{
				timer:      otherTimer,
				generation: 20,
				session:    Session{Key: otherKey, AgentName: "traex", ACPSessionID: "acp-b"},
				agent:      config.AgentConfig{Command: "traex"},
			}
			if tt.busy {
				svc.tasks[normalizedKey] = &runningTask{kind: taskKindUser}
			}
			svc.taskMu.Unlock()

			state := svc.beginWikiTimerRun(key, tt.generation)

			svc.taskMu.Lock()
			_, hasKey := svc.wikiTimers[normalizedKey]
			_, hasOther := svc.wikiTimers[otherKey]
			keyGen := svc.wikiGenerations[normalizedKey]
			otherGen := svc.wikiGenerations[otherKey]
			svc.taskMu.Unlock()
			if state != tt.wantState {
				t.Fatalf("state = %v, want %v", state, tt.wantState)
			}
			if hasKey != tt.wantHasKey {
				t.Fatalf("wiki timer exists = %v, want %v", hasKey, tt.wantHasKey)
			}
			if hasOther != tt.wantOther {
				t.Fatalf("other wiki timer exists = %v, want %v", hasOther, tt.wantOther)
			}
			if keyGen != tt.wantKeyGen {
				t.Fatalf("wiki generation = %d, want %d", keyGen, tt.wantKeyGen)
			}
			if otherGen != tt.wantOtherGen {
				t.Fatalf("other wiki generation = %d, want %d", otherGen, tt.wantOtherGen)
			}
		})
	}
}

func TestWikiStatusMarkersSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	otherKey := normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"})
	oldStarted := time.Date(2020, 8, 1, 11, 0, 0, 0, time.UTC)
	oldEnded := oldStarted.Add(time.Minute)
	newServiceWithStatuses := func(t *testing.T) *Service {
		t.Helper()
		svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
		svc.taskMu.Lock()
		svc.wikiStatuses[normalizedKey] = wikiRunStatus{
			running:     false,
			lastStarted: oldStarted,
			lastEnded:   oldEnded,
			lastSuccess: true,
			lastError:   "旧错误",
		}
		svc.wikiStatuses[otherKey] = wikiRunStatus{
			running:     true,
			lastStarted: oldStarted.Add(-time.Hour),
			lastEnded:   oldEnded.Add(-time.Hour),
			lastSuccess: false,
			lastError:   "其他 session 错误",
		}
		svc.taskMu.Unlock()
		return svc
	}
	cases := []struct {
		name            string
		run             func(svc *Service)
		wantRunning     bool
		wantSuccess     bool
		wantError       string
		wantEndedZero   bool
		wantStartedMove bool
	}{
		{
			name: "markWikiStarted 规范化 key 后进入 running 并清理上次结束状态",
			run: func(svc *Service) {
				svc.markWikiStarted(key)
			},
			wantRunning:     true,
			wantSuccess:     false,
			wantError:       "",
			wantEndedZero:   true,
			wantStartedMove: true,
		},
		{
			name: "markWikiFinished nil error 视为成功",
			run: func(svc *Service) {
				svc.markWikiFinished(key, Session{ACPSessionID: "acp-a"}, nil)
			},
			wantRunning:   false,
			wantSuccess:   true,
			wantError:     "",
			wantEndedZero: false,
		},
		{
			name: "markWikiFinished context canceled 视为成功",
			run: func(svc *Service) {
				svc.markWikiFinished(key, Session{ACPSessionID: "acp-a"}, context.Canceled)
			},
			wantRunning:   false,
			wantSuccess:   true,
			wantError:     "",
			wantEndedZero: false,
		},
		{
			name: "markWikiFinished 普通错误记录失败原因",
			run: func(svc *Service) {
				svc.markWikiFinished(key, Session{ACPSessionID: "acp-a"}, errors.New("wiki failed"))
			},
			wantRunning:   false,
			wantSuccess:   false,
			wantError:     "wiki failed",
			wantEndedZero: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newServiceWithStatuses(t)

			tt.run(svc)

			svc.taskMu.Lock()
			status := svc.wikiStatuses[normalizedKey]
			otherStatus := svc.wikiStatuses[otherKey]
			svc.taskMu.Unlock()
			if status.running != tt.wantRunning {
				t.Fatalf("running = %v, want %v", status.running, tt.wantRunning)
			}
			if status.lastSuccess != tt.wantSuccess {
				t.Fatalf("lastSuccess = %v, want %v", status.lastSuccess, tt.wantSuccess)
			}
			if status.lastError != tt.wantError {
				t.Fatalf("lastError = %q, want %q", status.lastError, tt.wantError)
			}
			if status.lastEnded.IsZero() != tt.wantEndedZero {
				t.Fatalf("lastEnded zero = %v, want %v", status.lastEnded.IsZero(), tt.wantEndedZero)
			}
			if tt.wantStartedMove && !status.lastStarted.After(oldStarted) {
				t.Fatalf("lastStarted = %s, want after %s", status.lastStarted, oldStarted)
			}
			if !tt.wantStartedMove && !status.lastStarted.Equal(oldStarted) {
				t.Fatalf("lastStarted = %s, want unchanged %s", status.lastStarted, oldStarted)
			}
			if otherStatus.lastError != "其他 session 错误" || !otherStatus.running {
				t.Fatalf("other status = %+v, want unchanged", otherStatus)
			}
		})
	}
}

func TestWikiTaskLifecycleSessionWorkBoundaries(t *testing.T) {
	key := SessionKey{BotID: "bot-a", ChatID: "oc_chat"}
	normalizedKey := normalizeSessionKey(key)
	runtime := wikiRuntimeKey(normalizedKey, 1, "acp-a")
	otherRuntime := wikiRuntimeKey(normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "oc_other"}), 1, "acp-b")
	cases := []struct {
		name           string
		replaceTask    bool
		finishRuntime  runtimeKey
		wantFinished   bool
		wantTargetTask string
		wantOtherTask  bool
	}{
		{
			name:           "匹配 runtime 和 task 时删除后台 wiki task",
			finishRuntime:  runtime,
			wantFinished:   true,
			wantTargetTask: "",
			wantOtherTask:  true,
		},
		{
			name:           "同 runtime 已替换 task 时旧 task finish 不删除新 task",
			replaceTask:    true,
			finishRuntime:  runtime,
			wantFinished:   false,
			wantTargetTask: "acp-new",
			wantOtherTask:  true,
		},
		{
			name:           "其他 runtime finish 不影响目标 task",
			finishRuntime:  otherRuntime,
			wantFinished:   false,
			wantTargetTask: "acp-a",
			wantOtherTask:  true,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(config.Default(), NewSessionStore(filepath.Join(t.TempDir(), "sessions.json")))
			task := &runningTask{
				kind:    taskKindWiki,
				runtime: runtime,
				session: Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-a"},
				agent:   config.AgentConfig{Command: "traex"},
			}
			otherTask := &runningTask{
				kind:    taskKindWiki,
				runtime: otherRuntime,
				session: Session{Key: otherRuntime.SessionKey, AgentName: "traex", ACPSessionID: "acp-b"},
				agent:   config.AgentConfig{Command: "traex"},
			}
			svc.beginWikiTask(runtime, task)
			svc.beginWikiTask(otherRuntime, otherTask)
			if tt.replaceTask {
				newTask := &runningTask{
					kind:    taskKindWiki,
					runtime: runtime,
					session: Session{Key: normalizedKey, AgentName: "traex", ACPSessionID: "acp-new"},
					agent:   config.AgentConfig{Command: "traex"},
				}
				svc.beginWikiTask(runtime, newTask)
			}

			finished := svc.finishWikiTask(tt.finishRuntime, task)

			svc.taskMu.Lock()
			targetTask := svc.wikiTasks[runtime]
			otherRemaining := svc.wikiTasks[otherRuntime]
			svc.taskMu.Unlock()
			if finished != tt.wantFinished {
				t.Fatalf("finishWikiTask() = %v, want %v", finished, tt.wantFinished)
			}
			if tt.wantTargetTask == "" {
				if targetTask != nil {
					t.Fatalf("target task = %+v, want nil", targetTask)
				}
			} else if targetTask == nil || targetTask.session.ACPSessionID != tt.wantTargetTask {
				t.Fatalf("target task = %+v, want acp session %q", targetTask, tt.wantTargetTask)
			}
			if (otherRemaining != nil) != tt.wantOtherTask {
				t.Fatalf("other task = %+v, want exists=%v", otherRemaining, tt.wantOtherTask)
			}
		})
	}
}

func TestCancelWikiTasksSessionBoundaries(t *testing.T) {
	agent := config.AgentConfig{Command: "traex"}
	cases := []struct {
		name                 string
		cancelKey            SessionKey
		includeSecondWiki    bool
		wantCanceledRuntimes []runtimeKey
		wantRemainingRuntime runtimeKey
	}{
		{
			name: "取消同 session 后台 wiki",
			cancelKey: SessionKey{
				BotID:  "bot-a",
				Source: "im",
				ChatID: "chat-a",
				MainID: "chat-a",
			},
			wantCanceledRuntimes: []runtimeKey{
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "1:acp-a",
				},
			},
			wantRemainingRuntime: runtimeKey{
				SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-b", MainID: "chat-b"},
				Scope:      runtimeScopeWiki,
				RunID:      "1:acp-b",
			},
		},
		{
			name: "规范化 key 后取消同 session 后台 wiki",
			cancelKey: SessionKey{
				BotID:  "bot-a",
				ChatID: "chat-a",
			},
			wantCanceledRuntimes: []runtimeKey{
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "1:acp-a",
				},
			},
			wantRemainingRuntime: runtimeKey{
				SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-b", MainID: "chat-b"},
				Scope:      runtimeScopeWiki,
				RunID:      "1:acp-b",
			},
		},
		{
			name: "取消同 session 下多个后台 wiki",
			cancelKey: SessionKey{
				BotID:  "bot-a",
				ChatID: "chat-a",
			},
			includeSecondWiki: true,
			wantCanceledRuntimes: []runtimeKey{
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "1:acp-a",
				},
				{
					SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-a", MainID: "chat-a"},
					Scope:      runtimeScopeWiki,
					RunID:      "2:acp-a2",
				},
			},
			wantRemainingRuntime: runtimeKey{
				SessionKey: SessionKey{BotID: "bot-a", Source: "im", ChatID: "chat-b", MainID: "chat-b"},
				Scope:      runtimeScopeWiki,
				RunID:      "1:acp-b",
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(config.Config{}, NewSessionStore(""))
			rt := &fakeRuntime{}
			svc.setRuntime(rt)
			sessionA := Session{
				Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-a"}),
				AgentName:    "traex",
				ACPSessionID: "acp-a",
			}
			sessionB := Session{
				Key:          normalizeSessionKey(SessionKey{BotID: "bot-a", ChatID: "chat-b"}),
				AgentName:    "traex",
				ACPSessionID: "acp-b",
			}
			sessionA2 := Session{
				Key:          sessionA.Key,
				AgentName:    "traex",
				ACPSessionID: "acp-a2",
			}
			runtimeA := wikiRuntimeKey(sessionA.Key, 1, sessionA.ACPSessionID)
			runtimeA2 := wikiRuntimeKey(sessionA2.Key, 2, sessionA2.ACPSessionID)
			runtimeB := wikiRuntimeKey(sessionB.Key, 1, sessionB.ACPSessionID)
			ctxA, finishA := svc.startWikiTask(context.Background(), sessionA, agent, runtimeA)
			var ctxA2 context.Context
			var finishA2 func()
			if tt.includeSecondWiki {
				ctxA2, finishA2 = svc.startWikiTask(context.Background(), sessionA2, agent, runtimeA2)
			}
			ctxB, finishB := svc.startWikiTask(context.Background(), sessionB, agent, runtimeB)
			t.Cleanup(finishA)
			if finishA2 != nil {
				t.Cleanup(finishA2)
			}
			t.Cleanup(finishB)

			svc.cancelWikiTasks(context.Background(), tt.cancelKey)
			select {
			case <-ctxA.Done():
				if !slices.Contains(tt.wantCanceledRuntimes, runtimeA) {
					t.Fatal("session A wiki task was cancelled unexpectedly")
				}
			default:
				if slices.Contains(tt.wantCanceledRuntimes, runtimeA) {
					t.Fatal("session A wiki task was not cancelled")
				}
			}
			if ctxA2 != nil {
				select {
				case <-ctxA2.Done():
					if !slices.Contains(tt.wantCanceledRuntimes, runtimeA2) {
						t.Fatal("session A second wiki task was cancelled unexpectedly")
					}
				default:
					if slices.Contains(tt.wantCanceledRuntimes, runtimeA2) {
						t.Fatal("session A second wiki task was not cancelled")
					}
				}
			}
			select {
			case <-ctxB.Done():
				if !slices.Contains(tt.wantCanceledRuntimes, runtimeB) {
					t.Fatal("session B wiki task was cancelled unexpectedly")
				}
			default:
				if slices.Contains(tt.wantCanceledRuntimes, runtimeB) {
					t.Fatal("session B wiki task was not cancelled")
				}
			}

			waitForCondition(t, time.Second, func() bool { return rt.cancelCallCount() == len(tt.wantCanceledRuntimes) })
			rt.mu.Lock()
			cancelCalls := append([]fakeCancelCall(nil), rt.cancelCalls...)
			rt.mu.Unlock()
			canceledRuntimeSet := make(map[runtimeKey]bool)
			for _, call := range cancelCalls {
				canceledRuntimeSet[call.Runtime] = true
			}
			for _, want := range tt.wantCanceledRuntimes {
				if !canceledRuntimeSet[want] {
					t.Fatalf("cancel runtimes = %+v, want include %+v", cancelCalls, want)
				}
			}
			svc.taskMu.Lock()
			hasCanceled := false
			for _, runtime := range tt.wantCanceledRuntimes {
				if _, ok := svc.wikiTasks[runtime]; ok {
					hasCanceled = true
					break
				}
			}
			_, hasRemaining := svc.wikiTasks[tt.wantRemainingRuntime]
			svc.taskMu.Unlock()
			if hasCanceled {
				t.Fatalf("wikiTasks still contains canceled runtime from %+v", tt.wantCanceledRuntimes)
			}
			if !hasRemaining {
				t.Fatalf("wikiTasks missing remaining runtime %+v", tt.wantRemainingRuntime)
			}
		})
	}
}

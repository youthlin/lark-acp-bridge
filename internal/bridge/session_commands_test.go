package bridge

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/youthlin/lark-acp-bridge/internal/acp"
	"github.com/youthlin/lark-acp-bridge/internal/config"
	"github.com/youthlin/lark-acp-bridge/internal/feishu"
)

func TestHandleFeishuSessionListSelectionOptionsAreLimited(t *testing.T) {
	items := make([]Session, 0, maxSessionHistoryPerChat+2)
	for i := 0; i < maxSessionHistoryPerChat+2; i++ {
		items = append(items, Session{
			Title:        fmt.Sprintf("会话%d", i),
			ACPSessionID: fmt.Sprintf("session-%d", i),
			Cwd:          "/repo",
		})
	}
	options := sessionSelectionOptions(items, maxSessionHistoryPerChat)
	if len(options) != maxSessionHistoryPerChat {
		t.Fatalf("len(options) = %d, want %d", len(options), maxSessionHistoryPerChat)
	}
	if options[0].ACPSessionID != "session-0" || options[len(options)-1].ACPSessionID != fmt.Sprintf("session-%d", maxSessionHistoryPerChat-1) {
		t.Fatalf("options = %+v, want first %d items", options, maxSessionHistoryPerChat)
	}
}

func TestFormatSessionListOnlyShowsCurrentIMHistory(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	imKey := imSessionKey("bot-a", "oc_chat", "")
	otherIMKey := imSessionKey("bot-a", "oc_other", "")
	scheduleKey := SessionKey{BotID: "bot-a", Source: "schedule", MainID: "oc_chat", SubID: "run:1"}
	commentKey := SessionKey{BotID: "bot-a", Source: "drive_comment", MainID: "oc_chat", SubID: "comment-1"}
	for _, session := range []Session{
		{Key: imKey, Title: "IM 会话", AgentName: "traex", ACPSessionID: "im-session", Cwd: "/im"},
		{Key: otherIMKey, Title: "其他 IM 会话", AgentName: "traex", ACPSessionID: "other-im-session", Cwd: "/other-im"},
		{Key: scheduleKey, Title: "Schedule 会话", AgentName: "traex", ACPSessionID: "schedule-session", Cwd: "/schedule"},
		{Key: commentKey, Title: "Drive 评论会话", AgentName: "traex", ACPSessionID: "comment-session", Cwd: "/comment"},
	} {
		if err := store.Upsert(session); err != nil {
			t.Fatalf("Upsert(%s) error = %v", session.ACPSessionID, err)
		}
	}
	svc := newTestService(config.Default(), store)

	reply := svc.formatSessionList(feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_chat",
		ChatType: "p2p",
	}, 0)
	if !strings.Contains(reply, "im-session") || !strings.Contains(reply, "IM 会话") {
		t.Fatalf("formatSessionList() = %q, want current IM history", reply)
	}
	for _, unexpected := range []string{"other-im-session", "schedule-session", "comment-session"} {
		if strings.Contains(reply, unexpected) {
			t.Fatalf("formatSessionList() = %q, should not include %s", reply, unexpected)
		}
	}
}

func TestHandleSessionSelectionRestoresSessionForOwnerOnly(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	rt := &fakeRuntime{newSessionIDs: []string{"acp-session-1", "acp-session-2"}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)
	base := feishu.Message{
		BotID:    "bot-a",
		ChatID:   "oc_private",
		ChatType: "p2p",
	}

	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_first",
		Text:      "/new " + firstDir + " 第一个",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new first) error = %v", err)
	}
	if _, err := handleFeishuMessage(t, svc, context.Background(), feishu.Message{
		BotID:     base.BotID,
		ChatID:    base.ChatID,
		ChatType:  base.ChatType,
		MessageID: "om_second",
		Text:      "/new " + secondDir + " 第二个",
	}); err != nil {
		t.Fatalf("HandleFeishuMessage(/new second) error = %v", err)
	}

	display, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:               base.BotID,
		ChatID:              base.ChatID,
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: "acp-session-2",
		ACPSessionID:        "acp-session-1",
	})
	if err != nil {
		t.Fatalf("HandleSessionSelection(owner) error = %v", err)
	}
	if display != "第一个" {
		t.Fatalf("display = %q, want restored title", display)
	}
	session, ok := store.Get(imSessionKey(base.BotID, base.ChatID, ""))
	if !ok {
		t.Fatalf("current session not found")
	}
	if session.ACPSessionID != "acp-session-1" || session.Cwd != firstDir {
		t.Fatalf("session = %+v, want first session restored", session)
	}
	if len(rt.closedKeys) == 0 {
		t.Fatalf("closedKeys = %+v, want runtime closed before resume", rt.closedKeys)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		OperatorID:   "ou_other",
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "只有发起") {
		t.Fatalf("other requester error = %v, want requester validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  "ou_other",
		OperatorID:   "ou_other",
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "只有 bot owner") {
		t.Fatalf("non-owner error = %v, want owner validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		OperatorID:   testOwnerOpenID,
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing requester error = %v, want requester metadata validation", err)
	}

	if _, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:        base.BotID,
		ChatID:       base.ChatID,
		RequesterID:  testOwnerOpenID,
		ACPSessionID: "acp-session-2",
	}); err == nil || !strings.Contains(err.Error(), "缺少发起人或操作者") {
		t.Fatalf("missing operator error = %v, want operator metadata validation", err)
	}
}

func TestHandleSessionSelectionRejectsStaleCard(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_private", "")
	for i := 1; i <= 3; i++ {
		if err := store.Upsert(Session{
			Key:          key,
			Title:        fmt.Sprintf("会话%d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-session-%d", i),
			Cwd:          fmt.Sprintf("/repo/%d", i),
		}); err != nil {
			t.Fatalf("Upsert(session %d) error = %v", i, err)
		}
	}
	rt := &fakeRuntime{}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:               key.BotID,
		ChatID:              sessionKeyMainID(key),
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: "acp-session-2",
		ACPSessionID:        "acp-session-1",
	})
	if err == nil || !strings.Contains(err.Error(), "当前会话已变化") {
		t.Fatalf("HandleSessionSelection(stale) error = %v, want stale card rejection", err)
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != "acp-session-3" {
		t.Fatalf("current after stale card = %+v, %v; want session 3", current, ok)
	}
	if len(rt.closedKeys) != 0 {
		t.Fatalf("closedKeys = %+v, want runtime unchanged", rt.closedKeys)
	}
}

func TestHandleSessionSelectionRestoresTopicSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_topic", "omt_topic")
	for i := 1; i <= 2; i++ {
		if err := store.Upsert(Session{
			Key:          key,
			Title:        fmt.Sprintf("话题会话%d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-topic-%d", i),
			Cwd:          fmt.Sprintf("/repo/%d", i),
		}); err != nil {
			t.Fatalf("Upsert(topic session %d) error = %v", i, err)
		}
	}
	rt := &fakeRuntime{activeSessionIDs: map[SessionKey]string{key: "acp-topic-2"}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	display, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:               key.BotID,
		ChatID:              sessionKeyMainID(key),
		ThreadID:            key.SubID,
		GroupMessageType:    "thread",
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: "acp-topic-2",
		ACPSessionID:        "acp-topic-1",
	})
	if err != nil {
		t.Fatalf("HandleSessionSelection(topic) error = %v", err)
	}
	if display != "话题会话1" {
		t.Fatalf("display = %q, want restored topic title", display)
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != "acp-topic-1" {
		t.Fatalf("current topic session = %+v, %v; want acp-topic-1", current, ok)
	}
	if _, ok := store.Get(imSessionKey(key.BotID, sessionKeyMainID(key), "")); ok {
		t.Fatal("topic card restore unexpectedly changed chat-level session")
	}
	if active := rt.activeSessionIDs[normalizeSessionKey(key)]; active != "acp-topic-1" {
		t.Fatalf("active runtime session = %q, want acp-topic-1 marker", active)
	}
}

func TestHandleSessionSelectionOrdinaryGroupThreadIDUsesChatSession(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_group", "")
	for i := 1; i <= 2; i++ {
		if err := store.Upsert(Session{
			Key:          key,
			Title:        fmt.Sprintf("群会话%d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-group-%d", i),
			Cwd:          fmt.Sprintf("/repo/%d", i),
		}); err != nil {
			t.Fatalf("Upsert(group session %d) error = %v", i, err)
		}
	}
	rt := &fakeRuntime{activeSessionIDs: map[SessionKey]string{key: "acp-group-2"}}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	_, err := svc.HandleSessionSelection(context.Background(), feishu.SessionSelection{
		BotID:               key.BotID,
		ChatID:              sessionKeyMainID(key),
		ThreadID:            "omt_reply_thread",
		GroupMessageType:    "chat",
		RequesterID:         testOwnerOpenID,
		OperatorID:          testOwnerOpenID,
		CurrentACPSessionID: "acp-group-2",
		ACPSessionID:        "acp-group-1",
	})
	if err != nil {
		t.Fatalf("HandleSessionSelection(ordinary group) error = %v", err)
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != "acp-group-1" {
		t.Fatalf("current group session = %+v, %v; want acp-group-1", current, ok)
	}
	if _, ok := store.Get(imSessionKey(key.BotID, sessionKeyMainID(key), "omt_reply_thread")); ok {
		t.Fatal("ordinary group card restore unexpectedly created a topic session")
	}
}

func TestResumeSessionSucceedsWhenOldRuntimeCloseFails(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_private", "")
	for i := 1; i <= 2; i++ {
		if err := store.Upsert(Session{
			Key:          key,
			Title:        fmt.Sprintf("会话%d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-session-%d", i),
			Cwd:          fmt.Sprintf("/repo/%d", i),
		}); err != nil {
			t.Fatalf("Upsert(session %d) error = %v", i, err)
		}
	}
	rt := &fakeRuntime{
		activeSessionIDs:   map[SessionKey]string{key: "acp-session-2"},
		transitionCloseErr: fmt.Errorf("close failed"),
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	reply := svc.resumeSession(context.Background(), feishu.Message{
		BotID:    key.BotID,
		ChatID:   sessionKeyMainID(key),
		ChatType: "p2p",
	}, 2)
	if !strings.Contains(reply, "已恢复会话 2") || !strings.Contains(reply, "session：acp-session-1") {
		t.Fatalf("resumeSession() = %q, want success despite close error", reply)
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != "acp-session-1" {
		t.Fatalf("current session = %+v, %v; want acp-session-1", current, ok)
	}
}

func TestResumeSessionDoesNotCloseConcurrentlyCreatedRuntime(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	key := imSessionKey("bot-a", "oc_private", "")
	for i := 1; i <= 2; i++ {
		if err := store.Upsert(Session{
			Key:          key,
			Title:        fmt.Sprintf("会话%d", i),
			AgentName:    "traex",
			ACPSessionID: fmt.Sprintf("acp-session-%d", i),
			Cwd:          fmt.Sprintf("/repo/%d", i),
		}); err != nil {
			t.Fatalf("Upsert(session %d) error = %v", i, err)
		}
	}
	transitionStarted := make(chan struct{})
	releaseTransition := make(chan struct{})
	rt := &fakeRuntime{
		newSessionID:     "acp-session-3",
		activeSessionIDs: map[SessionKey]string{key: "acp-session-2"},
		transitionBefore: func() {
			close(transitionStarted)
			<-releaseTransition
		},
	}
	svc := newTestService(config.Default(), store)
	svc.setRuntime(rt)

	resumeDone := make(chan string, 1)
	go func() {
		resumeDone <- svc.resumeSession(context.Background(), feishu.Message{
			BotID:    key.BotID,
			ChatID:   sessionKeyMainID(key),
			ChatType: "p2p",
		}, 2)
	}()
	<-transitionStarted

	candidate, err := rt.NewSession(
		context.Background(),
		key,
		"traex",
		mustConfigAgent(t, config.Default(), "traex"),
		"/repo/3",
		"",
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	newSession := Session{
		Key:          key,
		Title:        "会话3",
		AgentName:    "traex",
		ACPSessionID: candidate.Info().SessionID,
		Cwd:          "/repo/3",
	}
	commitStarted := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		close(commitStarted)
		commitDone <- candidate.Commit(func() error {
			return store.Upsert(newSession)
		})
	}()
	<-commitStarted
	select {
	case err := <-commitDone:
		t.Fatalf("candidate commit completed before resume transition released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseTransition)
	if reply := <-resumeDone; !strings.Contains(reply, "已恢复会话 2") {
		t.Fatalf("resumeSession() = %q, want session 1 restored before concurrent /new", reply)
	}
	if err := <-commitDone; err != nil {
		t.Fatalf("candidate Commit() error = %v", err)
	}
	current, ok := store.Get(key)
	if !ok || current.ACPSessionID != newSession.ACPSessionID {
		t.Fatalf("current session = %+v, %v; want concurrent new session", current, ok)
	}
	if active := rt.activeSessionIDs[normalizeSessionKey(key)]; active != newSession.ACPSessionID {
		t.Fatalf("active runtime session = %q, want concurrent new session", active)
	}
}

func TestSetSessionTitleOnlyUpdatesCurrentSessionTitle(t *testing.T) {
	store := NewSessionStore(filepath.Join(t.TempDir(), "sessions.json"))
	stale := testReadySession(t, store)
	latest := stale
	latest.AvailableCommands = []acp.AvailableCommand{{Name: "review"}}
	if err := store.Upsert(latest); err != nil {
		t.Fatalf("Upsert(latest) error = %v", err)
	}
	svc := newTestService(config.Default(), store)
	msg := feishu.Message{
		BotID:            stale.Key.BotID,
		ChatID:           sessionKeyMainID(stale.Key),
		ThreadID:         stale.Key.SubID,
		GroupMessageType: "thread",
	}

	reply := svc.setSessionTitle(context.Background(), msg, "新标题")
	if !strings.Contains(reply, "已设置当前会话标题：新标题") {
		t.Fatalf("setSessionTitle() = %q, want success", reply)
	}
	updated, ok := store.Get(stale.Key)
	if !ok {
		t.Fatalf("updated session not found")
	}
	if updated.Title != "新标题" || !updated.ManualTitle || !sessionHasCommand(updated, "review") {
		t.Fatalf("updated session = %+v, want title update and latest commands preserved", updated)
	}

	newSession := latest
	newSession.ACPSessionID = "acp-session-new"
	newSession.Title = "新会话"
	newSession.ManualTitle = false
	if err := store.Upsert(newSession); err != nil {
		t.Fatalf("Upsert(newSession) error = %v", err)
	}
	_, updatedCurrent, err := store.UpdateManualTitle(stale.Key, stale.ACPSessionID, "过期标题")
	if err != nil {
		t.Fatalf("UpdateManualTitle(stale) error = %v", err)
	}
	if updatedCurrent {
		t.Fatalf("UpdateManualTitle(stale) updated current session")
	}
	persisted, ok := store.Get(stale.Key)
	if !ok || persisted.ACPSessionID != newSession.ACPSessionID || persisted.Title != newSession.Title || persisted.ManualTitle {
		t.Fatalf("session after stale title update = %+v, %v; want new session unchanged", persisted, ok)
	}
}

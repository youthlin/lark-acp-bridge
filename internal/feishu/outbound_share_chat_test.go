package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestSendShareChatMessageRepliesInThread(t *testing.T) {
	type replyBody struct {
		Content       string `json:"content"`
		MsgType       string `json:"msg_type"`
		ReplyInThread bool   `json:"reply_in_thread"`
	}
	var got replyBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "tenant_access_token"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"test-token","expire":7200}`))
		case request.URL.Path == "/open-apis/im/v1/messages/om_source/reply":
			if request.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", request.Method)
			}
			if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
				t.Errorf("decode reply body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_share","root_id":"om_source","thread_id":"omt_source","chat_id":"oc_source","msg_type":"share_chat"}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	adapter := &Adapter{client: lark.NewClient("cli_test", "secret", lark.WithOpenBaseUrl(server.URL))}
	sent, err := adapter.SendShareChatMessage(context.Background(), Message{
		MessageID:          "om_source",
		ChatID:             "oc_source",
		ChatType:           "group",
		ForceReplyInThread: true,
	}, "oc_target")
	if err != nil {
		t.Fatalf("SendShareChatMessage() error = %v", err)
	}
	if got.MsgType != "share_chat" || !got.ReplyInThread || got.Content != `{"chat_id":"oc_target"}` {
		t.Fatalf("reply body = %+v, want share_chat threaded reply", got)
	}
	if sent.MessageID != "om_share" || sent.ChatID != "oc_source" || sent.ThreadID != "omt_source" {
		t.Fatalf("sent message = %+v, want response metadata", sent)
	}
}

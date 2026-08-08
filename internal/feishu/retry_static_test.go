package feishu

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestNonIdempotentMessageSendPathsDoNotUseRetryHelper(t *testing.T) {
	files := []string{
		"outbound_message.go",
		"cardkit_client.go",
		"message_client.go",
		"permission_card.go",
		"reaction_client.go",
	}
	functions := map[string]bool{
		"SendChatTextMessage":         true,
		"SendChatPostMessage":         true,
		"ReplyTextMessage":            true,
		"ReplyPostMessage":            true,
		"SendChatImageMessage":        true,
		"ReplyImageMessage":           true,
		"UploadImage":                 true,
		"sendInteractiveCard":         true,
		"sendInteractiveCardToOpenID": true,
		"AddReaction":                 true,
	}
	seen := map[string]bool{}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s) error = %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !functions[fn.Name.Name] {
				continue
			}
			seen[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "retryFeishuAPI" {
					t.Fatalf("%s must not call retryFeishuAPI; Message.Create/Reply paths are not retried without a dedupe key", fn.Name.Name)
				}
				return true
			})
		}
	}
	for name := range functions {
		if !seen[name] {
			t.Fatalf("did not inspect %s; update static retry guard", name)
		}
	}
}

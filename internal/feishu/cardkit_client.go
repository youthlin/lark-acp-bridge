package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type cardJSON map[string]any

func (a *Adapter) createCardJSON(ctx context.Context, data string, name string) (string, error) {
	displayName := cardNameForError(name)
	cardResp, err := a.client.Cardkit.V1.Card.Create(ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(data).
			Build()).
		Build())
	if err != nil {
		return "", fmt.Errorf("创建飞书%s卡片: %w", displayName, err)
	}
	if !cardResp.Success() {
		return "", fmt.Errorf("创建飞书%s卡片返回错误: code=%d msg=%s", displayName, cardResp.Code, cardResp.Msg)
	}
	cardID := ""
	if cardResp.Data != nil {
		cardID = normalizedCardID(cardResp.Data.CardId)
	}
	if cardID == "" {
		return "", fmt.Errorf("创建飞书%s卡片未返回 card_id", displayName)
	}
	return cardID, nil
}

func (a *Adapter) createAndSendCardJSON(ctx context.Context, msg Message, data string, name string) (string, SentMessage, error) {
	cardID, err := a.createCardJSON(ctx, data, name)
	if err != nil {
		return "", SentMessage{}, err
	}
	sent, err := a.sendInteractiveCard(ctx, msg, cardID, name)
	if err != nil {
		return "", SentMessage{}, err
	}
	return cardID, sent, nil
}

func cardNameForError(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if cardNameNeedsSpaceAfterFeishu(name) {
		return " " + name
	}
	return name
}

func cardNameNeedsSpaceAfterFeishu(name string) bool {
	first, _ := utf8.DecodeRuneInString(name)
	return first >= 'A' && first <= 'Z' || first >= 'a' && first <= 'z'
}

func (a *Adapter) sendInteractiveCard(ctx context.Context, msg Message, cardID string, name string) (SentMessage, error) {
	displayName := cardNameForError(name)
	content, err := json.Marshal(map[string]any{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	if err != nil {
		return SentMessage{}, fmt.Errorf("编码飞书%s卡片消息内容: %w", displayName, err)
	}
	if strings.TrimSpace(msg.MessageID) == "" {
		if msg.ChatID == "" {
			return SentMessage{}, fmt.Errorf("发送飞书%s卡片消息: 飞书 chat_id 为空", displayName)
		}
		resp, err := a.client.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
			Body(larkim.NewCreateMessageReqBodyBuilder().
				ReceiveId(msg.ChatID).
				MsgType("interactive").
				Content(string(content)).
				Build()).
			Build())
		if err != nil {
			return SentMessage{}, fmt.Errorf("发送飞书%s卡片消息: %w", displayName, err)
		}
		if !resp.Success() {
			return SentMessage{}, fmt.Errorf("发送飞书%s卡片消息返回错误: code=%d msg=%s", displayName, resp.Code, resp.Msg)
		}
		return sentMessageFromCreateResp(resp, msg.ChatID, msg.ChatType), nil
	}
	if msg.MessageID == "" {
		return SentMessage{}, fmt.Errorf("回复飞书%s卡片消息: 飞书 message_id 为空", displayName)
	}
	replyInThread := replyInThreadForMessage(msg)
	resp, err := a.client.Im.Message.Reply(ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(msg.MessageID).
		Body(&larkim.ReplyMessageReqBody{
			Content:       larkcore.StringPtr(string(content)),
			MsgType:       larkcore.StringPtr("interactive"),
			ReplyInThread: &replyInThread,
		}).
		Build())
	if err != nil {
		return SentMessage{}, fmt.Errorf("回复飞书%s卡片消息: %w", displayName, err)
	}
	if !resp.Success() {
		return SentMessage{}, fmt.Errorf("回复飞书%s卡片消息返回错误: code=%d msg=%s", displayName, resp.Code, resp.Msg)
	}
	return sentMessageFromReplyResp(resp, msg), nil
}

type cardUpdateRequest struct {
	cardID   string
	data     string
	sequence int
	action   string
	log      bool
}

func (a *Adapter) updateCardJSON(ctx context.Context, req cardUpdateRequest) error {
	body := larkcardkit.NewUpdateCardReqBodyBuilder().
		Card(larkcardkit.NewCardBuilder().
			Type("card_json").
			Data(req.data).
			Build())
	if req.sequence > 0 {
		body.Sequence(req.sequence)
	}
	resp, err := a.client.Cardkit.V1.Card.Update(ctx, larkcardkit.NewUpdateCardReqBuilder().
		CardId(req.cardID).
		Body(body.Build()).
		Build())
	if err != nil {
		return fmt.Errorf("%s: %w", req.action, err)
	}
	if !resp.Success() {
		if req.log {
			logCardKitFailure(ctx, "Card.Update", req.cardID, "", req.sequence, cardJSON{
				"card": cardJSON{"type": "card_json", "data": req.data},
			}, resp.ApiResp, resp.Code, resp.Msg)
		}
		return fmt.Errorf("%s返回错误: code=%d msg=%s", req.action, resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) createCardElementsAfter(ctx context.Context, cardID string, targetElementID string, createdElementID string, elements string, sequence int, action string) error {
	resp, err := a.client.Cardkit.V1.CardElement.Create(ctx, larkcardkit.NewCreateCardElementReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewCreateCardElementReqBodyBuilder().
			Type(larkcardkit.TypeInsertAfter).
			TargetElementId(targetElementID).
			Elements(elements).
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "CardElement.Create", cardID, createdElementID, sequence, cardJSON{
			"type":              larkcardkit.TypeInsertAfter,
			"target_element_id": targetElementID,
			"elements":          elements,
		}, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("%s返回错误: code=%d msg=%s", action, resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) updateCardElementContent(ctx context.Context, cardID string, elementID string, content string, sequence int, action string) error {
	resp, err := a.client.Cardkit.V1.CardElement.Content(ctx, larkcardkit.NewContentCardElementReqBuilder().
		CardId(cardID).
		ElementId(elementID).
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().
			Content(content).
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "CardElement.Content", cardID, elementID, sequence, cardJSON{
			"content": content,
		}, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("%s返回错误: code=%d msg=%s", action, resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) patchCardElement(ctx context.Context, cardID string, elementID string, partial string, sequence int, action string) error {
	resp, err := a.client.Cardkit.V1.CardElement.Patch(ctx, larkcardkit.NewPatchCardElementReqBuilder().
		CardId(cardID).
		ElementId(elementID).
		Body(larkcardkit.NewPatchCardElementReqBodyBuilder().
			PartialElement(partial).
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "CardElement.Patch", cardID, elementID, sequence, cardJSON{
			"partial_element": partial,
		}, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("%s返回错误: code=%d msg=%s", action, resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) deleteCardElement(ctx context.Context, cardID string, elementID string, sequence int, action string) error {
	resp, err := a.client.Cardkit.V1.CardElement.Delete(ctx, larkcardkit.NewDeleteCardElementReqBuilder().
		CardId(cardID).
		ElementId(elementID).
		Body(larkcardkit.NewDeleteCardElementReqBodyBuilder().
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "CardElement.Delete", cardID, elementID, sequence, nil, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("%s返回错误: code=%d msg=%s", action, resp.Code, resp.Msg)
	}
	return nil
}

func (a *Adapter) updateCardSettings(ctx context.Context, cardID string, settings string, sequence int, action string) error {
	resp, err := a.client.Cardkit.V1.Card.Settings(ctx, larkcardkit.NewSettingsCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().
			Settings(settings).
			Sequence(sequence).
			Build()).
		Build())
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	if !resp.Success() {
		logCardKitFailure(ctx, "Card.Settings", cardID, "", sequence, cardJSON{
			"settings": settings,
		}, resp.ApiResp, resp.Code, resp.Msg)
		return fmt.Errorf("%s返回错误: code=%d msg=%s", action, resp.Code, resp.Msg)
	}
	return nil
}

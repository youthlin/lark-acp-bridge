package acp

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *RequestID      `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type RequestID struct {
	raw json.RawMessage
}

func NewRequestID(id int64) *RequestID {
	return &RequestID{raw: json.RawMessage(strconv.FormatInt(id, 10))}
}

func (id *RequestID) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return fmt.Errorf("json-rpc id cannot be null")
	}
	var stringID string
	if err := json.Unmarshal(data, &stringID); err == nil {
		id.raw = append(id.raw[:0], data...)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err == nil {
		id.raw = append(id.raw[:0], data...)
		return nil
	}
	return fmt.Errorf("json-rpc id must be string or number")
}

func (id RequestID) MarshalJSON() ([]byte, error) {
	if len(id.raw) == 0 {
		return nil, fmt.Errorf("empty json-rpc id")
	}
	return append([]byte(nil), id.raw...), nil
}

func (id *RequestID) Key() string {
	if id == nil {
		return ""
	}
	return string(id.raw)
}

func NewRequest(id int64, method string, params any) (Message, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return Message{}, err
	}
	return Message{
		JSONRPC: "2.0",
		ID:      NewRequestID(id),
		Method:  method,
		Params:  raw,
	}, nil
}

func NewNotification(method string, params any) (Message, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return Message{}, err
	}
	return Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	}, nil
}

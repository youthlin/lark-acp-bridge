package arg

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

func JSON(data any) *Arg {
	return &Arg{data: data}
}

func RawJSON(data []byte) *Arg {
	return &Arg{data: json.RawMessage(data)}
}

type Arg struct {
	data any
}

func (a *Arg) String() string {
	b, err := json.Marshal(a.data)
	if err != nil {
		return fmt.Sprintf("<ErrToJSON|err=%s|data=%#v>", err, a.data)
	}
	return B2s(b)
}

func (a *Arg) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.data)
}

func B2s(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func S2b(s string) []byte {
	if len(s) == 0 {
		return []byte(s) // 直接转换为 []byte 返回
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

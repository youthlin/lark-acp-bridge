package bridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type wikiTraceRange struct {
	FromSeq  uint64
	ToSeq    uint64
	Terminal traceRecord
}

func readWikiTraceRange(path string, fromSeq, upperSeq uint64) (wikiTraceRange, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return wikiTraceRange{}, fmt.Errorf("trace 文件不存在: %s", path)
		}
		return wikiTraceRange{}, err
	}
	defer file.Close()

	rangeInfo := wikiTraceRange{FromSeq: fromSeq}
	seenUser := make(map[string]bool)
	cursorFound := fromSeq == 0
	lineNumber := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		lineNumber++
		var record traceRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return wikiTraceRange{}, fmt.Errorf("解析 trace 第 %d 行: %w", lineNumber, err)
		}
		if record.Seq == 0 || record.Seq <= fromSeq || (upperSeq > 0 && record.Seq > upperSeq) {
			if record.Seq == fromSeq {
				cursorFound = true
			}
			continue
		}
		if strings.EqualFold(strings.TrimSpace(record.Source), "wiki") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(record.MessageID)), "wiki_") {
			continue
		}
		turnID := strings.TrimSpace(record.MessageID)
		if turnID == "" {
			turnID = "__empty__"
		}
		if record.Type == "user" {
			seenUser[turnID] = true
		}
		if record.Type == "turn_result" || record.Type == "error" {
			if !seenUser[turnID] {
				continue
			}
			rangeInfo.ToSeq = record.Seq
			rangeInfo.Terminal = record
		}
	}
	if err := scanner.Err(); err != nil {
		return wikiTraceRange{}, fmt.Errorf("读取 trace: %w", err)
	}
	if !cursorFound {
		return wikiTraceRange{}, fmt.Errorf("trace 已缺失 committed_seq=%d 对应记录", fromSeq)
	}
	return rangeInfo, nil
}

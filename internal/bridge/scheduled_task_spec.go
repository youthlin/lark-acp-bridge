package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type scheduleSpec interface {
	Next(time.Time) (time.Time, bool)
}

type everyScheduleSpec struct {
	interval time.Duration
}

func (s everyScheduleSpec) Next(after time.Time) (time.Time, bool) {
	if s.interval <= 0 {
		return time.Time{}, false
	}
	if after.IsZero() {
		after = time.Now()
	}
	return after.Add(s.interval), true
}

// onceScheduleSpec 表示只触发一次的任务：在 at 之前调用 Next 返回 at；
// 到点之后返回 ok=false，调度循环据此结束并删除任务。
type onceScheduleSpec struct {
	at time.Time
}

func (s onceScheduleSpec) Next(after time.Time) (time.Time, bool) {
	if after.IsZero() {
		after = time.Now()
	}
	if after.Before(s.at) {
		return s.at, true
	}
	return time.Time{}, false
}

type cronScheduleSpec struct {
	minute     cronField
	hour       cronField
	dayOfMonth cronField
	month      cronField
	weekday    cronField
	location   *time.Location
}

func (s cronScheduleSpec) Next(after time.Time) (time.Time, bool) {
	loc := s.location
	if loc == nil {
		loc = time.Local
	}
	if after.IsZero() {
		after = time.Now()
	}
	start := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	deadline := start.AddDate(5, 0, 0)
	for t := start; !t.After(deadline); {
		if !s.month.Matches(int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.matchesDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.hour.Matches(t.Hour()) {
			t = t.Add(time.Hour).Truncate(time.Hour)
			continue
		}
		if !s.minute.Matches(t.Minute()) {
			t = t.Add(time.Minute)
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

func (s cronScheduleSpec) matchesDay(t time.Time) bool {
	dayOfMonthMatches := s.dayOfMonth.Matches(t.Day())
	weekdayMatches := s.weekday.Matches(int(t.Weekday()))
	switch {
	case s.dayOfMonth.all && s.weekday.all:
		return true
	case s.dayOfMonth.all:
		return weekdayMatches
	case s.weekday.all:
		return dayOfMonthMatches
	default:
		return dayOfMonthMatches || weekdayMatches
	}
}

type cronField struct {
	min int
	max int
	all bool
	set map[int]struct{}
}

func (f cronField) Matches(value int) bool {
	if f.all {
		return true
	}
	_, ok := f.set[value]
	return ok
}

func parseScheduleSpec(spec string, timezone string) (scheduleSpec, error) {
	spec = strings.TrimSpace(spec)
	timezone = strings.TrimSpace(timezone)
	if spec == "" {
		return nil, fmt.Errorf("定时任务 spec 不能为空")
	}
	if strings.HasPrefix(spec, "@every ") {
		intervalText := strings.TrimSpace(strings.TrimPrefix(spec, "@every "))
		interval, err := time.ParseDuration(intervalText)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("解析 @every 间隔: %w", err)
		}
		return everyScheduleSpec{interval: interval}, nil
	}
	if strings.HasPrefix(spec, "@at ") {
		return parseOnceAtSpec(strings.TrimSpace(strings.TrimPrefix(spec, "@at ")), timezone)
	}
	loc := time.Local
	if timezone != "" {
		loaded, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, fmt.Errorf("解析定时任务 timezone: %w", err)
		}
		loc = loaded
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("定时任务 spec 仅支持 @every <duration>、@at <时间> 或 5 段 cron")
	}
	minute, err := parseCronField(parts[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("解析 cron minute: %w", err)
	}
	hour, err := parseCronField(parts[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("解析 cron hour: %w", err)
	}
	dayOfMonth, err := parseCronField(parts[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("解析 cron day of month: %w", err)
	}
	month, err := parseCronField(parts[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("解析 cron month: %w", err)
	}
	weekday, err := parseCronField(parts[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("解析 cron weekday: %w", err)
	}
	if _, ok := weekday.set[7]; ok {
		delete(weekday.set, 7)
		weekday.set[0] = struct{}{}
	}
	return cronScheduleSpec{minute: minute, hour: hour, dayOfMonth: dayOfMonth, month: month, weekday: weekday, location: loc}, nil
}

// parseOnceAtSpec 解析一次性任务的 @at 时间。
// 支持 "2006-01-02 15:04"（按 timezone，缺省 local）和 RFC3339（如 2026-08-05T09:00:00+08:00）。
func parseOnceAtSpec(value string, timezone string) (scheduleSpec, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("@at 时间不能为空")
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return onceScheduleSpec{at: t}, nil
	}
	loc := time.Local
	if strings.TrimSpace(timezone) != "" {
		loaded, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, fmt.Errorf("解析定时任务 timezone: %w", err)
		}
		loc = loaded
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		return nil, fmt.Errorf("解析 @at 时间（需为 RFC3339 或 2006-01-02 15:04）: %w", err)
	}
	return onceScheduleSpec{at: t}, nil
}

func parseCronField(expr string, min int, max int) (cronField, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return cronField{}, fmt.Errorf("字段为空")
	}
	field := cronField{min: min, max: max, set: make(map[int]struct{})}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return cronField{}, fmt.Errorf("包含空片段")
		}
		rangePart := part
		step := 1
		if strings.Contains(part, "/") {
			pieces := strings.Split(part, "/")
			if len(pieces) != 2 {
				return cronField{}, fmt.Errorf("无效步长 %q", part)
			}
			rangePart = strings.TrimSpace(pieces[0])
			parsedStep, err := strconv.Atoi(strings.TrimSpace(pieces[1]))
			if err != nil || parsedStep <= 0 {
				return cronField{}, fmt.Errorf("无效步长 %q", part)
			}
			step = parsedStep
		}
		start, end, all, err := parseCronRange(rangePart, min, max)
		if err != nil {
			return cronField{}, err
		}
		if all && step == 1 {
			field.all = true
			field.set = nil
			return field, nil
		}
		for value := start; value <= end; value += step {
			field.set[value] = struct{}{}
		}
	}
	return field, nil
}

func parseCronRange(expr string, min int, max int) (int, int, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" {
		return min, max, true, nil
	}
	if strings.Contains(expr, "-") {
		pieces := strings.Split(expr, "-")
		if len(pieces) != 2 {
			return 0, 0, false, fmt.Errorf("无效范围 %q", expr)
		}
		start, err := parseCronNumber(strings.TrimSpace(pieces[0]), min, max)
		if err != nil {
			return 0, 0, false, err
		}
		end, err := parseCronNumber(strings.TrimSpace(pieces[1]), min, max)
		if err != nil {
			return 0, 0, false, err
		}
		if start > end {
			return 0, 0, false, fmt.Errorf("无效倒序范围 %q", expr)
		}
		return start, end, false, nil
	}
	value, err := parseCronNumber(expr, min, max)
	if err != nil {
		return 0, 0, false, err
	}
	return value, value, false, nil
}

func parseCronNumber(expr string, min int, max int) (int, error) {
	value, err := strconv.Atoi(expr)
	if err != nil {
		return 0, fmt.Errorf("无效数字 %q", expr)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("数字 %d 超出范围 %d-%d", value, min, max)
	}
	return value, nil
}

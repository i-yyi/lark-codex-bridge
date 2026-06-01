package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type logLevel int

const (
	levelDebug logLevel = iota
	levelInfo
	levelError
)

func parseLogLevel(value string) logLevel {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return levelDebug
	case "error":
		return levelError
	default:
		return levelInfo
	}
}

func (app *daemon) log(event string, fields ...any) {
	app.logAt(levelInfo, event, fields...)
}

func (app *daemon) debug(event string, fields ...any) {
	app.logAt(levelDebug, event, fields...)
}

func (app *daemon) logError(event string, fields ...any) {
	app.logAt(levelError, event, fields...)
}

func (app *daemon) logAt(level logLevel, event string, fields ...any) {
	if level < app.level {
		return
	}
	app.logger.Printf("%-20s %s", event, formatLogFields(fields...))
}

func formatLogFields(fields ...any) string {
	parts := make([]string, 0, (len(fields)+1)/2)
	for index := 0; index < len(fields); index += 2 {
		key := fmt.Sprint(fields[index])
		value := ""
		if index+1 < len(fields) {
			value = formatLogValue(fields[index+1])
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, " ")
}

func formatLogValue(value any) string {
	if value == nil {
		return "-"
	}
	switch typed := value.(type) {
	case time.Duration:
		return typed.Round(time.Millisecond).String()
	case error:
		return quoteLogString(typed.Error())
	case fmt.Stringer:
		return quoteLogString(typed.String())
	case string:
		return quoteLogString(typed)
	default:
		return quoteLogString(fmt.Sprint(value))
	}
}

func quoteLogString(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if value == "" {
		return "-"
	}
	if strings.ContainsAny(value, " \t\"") {
		return fmt.Sprintf("%q", value)
	}
	return value
}

func elapsed(start time.Time) time.Duration {
	return time.Since(start)
}

func firstString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func parseEventMillis(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.UnixMilli(millis), true
}

func eventLag(from string, to string) any {
	fromTime, ok := parseEventMillis(from)
	if !ok {
		return nil
	}
	toTime, ok := parseEventMillis(to)
	if !ok {
		return nil
	}
	return toTime.Sub(fromTime)
}

func recvLag(eventTime string, receivedAt time.Time) any {
	createdAt, ok := parseEventMillis(eventTime)
	if !ok {
		return nil
	}
	return receivedAt.Sub(createdAt)
}

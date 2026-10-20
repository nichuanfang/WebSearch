// Package logging 提供只写 stderr 的结构化日志。
//
// stdio 传输下 stdout 是 MCP 的 JSON-RPC 通道，任何写入都会破坏协议帧，
// 因此本包的所有输出都强制走 stderr。
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// New 构造一个写往 stderr 的 slog.Logger。level 支持 debug/info/warn/error。
func New(level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))
}

// NewWithWriter 构造写往指定 io.Writer 的 logger，供测试使用。
func NewWithWriter(w io.Writer, level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

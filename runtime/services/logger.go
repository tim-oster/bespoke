package services

import (
	"log/slog"
	"os"

	"github.com/tim-oster/bespoke/runtime/slogctx"
)

func newLogger(level slog.Level) *slog.Logger {
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)

	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource:   false,
		Level:       levelVar,
		ReplaceAttr: nil,
	})
	return slog.New(slogctx.NewHandler(h))
}

func getLogLevel() slog.Level {
	logLevel := os.Getenv("BESPOKE_LOG_LEVEL")
	levelMap := map[string]slog.Level{
		"DEBUG": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"WARN":  slog.LevelWarn,
		"ERROR": slog.LevelError,
	}
	if lvl, ok := levelMap[logLevel]; ok {
		return lvl
	}
	return slog.LevelInfo
}

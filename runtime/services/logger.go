package services

import (
	"log/slog"
	"os"
)

type logHandlerWrapper func(slog.Handler) slog.Handler

func newLogger(level slog.Level, attrReplacer func(groups []string, a slog.Attr) slog.Attr, handlers []logHandlerWrapper) *slog.Logger {
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)

	var h slog.Handler
	h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource:   false,
		Level:       levelVar,
		ReplaceAttr: attrReplacer,
	})

	// reverse order to ensure that first handler is invoked first
	for i := range handlers {
		h = handlers[len(handlers)-i](h)
	}

	return slog.New(h)
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

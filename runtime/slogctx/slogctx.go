package slogctx

import (
	"context"
	"log/slog"
)

var _ slog.Handler = (*handler)(nil)

type handler struct {
	handler slog.Handler
}

func NewHandler(h slog.Handler) slog.Handler {
	return &handler{handler: h}
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := Get(ctx); attrs != nil && len(*attrs) > 0 {
		r.AddAttrs(*attrs...)
	}
	return h.handler.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return NewHandler(h.handler.WithAttrs(attrs))
}

func (h *handler) WithGroup(name string) slog.Handler {
	return NewHandler(h.handler.WithGroup(name))
}

type contextKey string

var attrContextKey = contextKey("attrs")

func PrepareContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, attrContextKey, &[]slog.Attr{})
}

func With(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing := Get(ctx)
	if existing == nil {
		existing = &[]slog.Attr{}
		ctx = context.WithValue(ctx, attrContextKey, existing)
	}
	*existing = append(*existing, attrs...)
	return ctx
}

func Get(ctx context.Context) *[]slog.Attr {
	attrs, ok := ctx.Value(attrContextKey).(*[]slog.Attr)
	if !ok {
		return nil
	}
	return attrs
}

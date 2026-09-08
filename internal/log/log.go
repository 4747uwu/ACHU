// Package log wires up the application's structured logger.
//
// Uses golang.org/x/exp/slog (the pre-1.21 backport of log/slog) so the binary
// can still be built with the last Windows 7-capable Go toolchain (1.20). The
// API is identical to stdlib log/slog. Output goes to stdout (so Electron can
// pipe it for the Logs tab) and to a rotating file under <data_dir>/logs/.
package log

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	slog "golang.org/x/exp/slog"
)

var (
	defaultMu     sync.Mutex
	defaultLogger *slog.Logger

	// errorSink receives a copy of every error-or-higher log record once set.
	// Stored atomically so the logging hot path never takes a lock.
	errorSink atomic.Pointer[func(ErrorEvent)]
)

// ErrorEvent is a captured error-level (or higher) log record handed to the
// sink registered via SetErrorSink. It carries the message and a flattened
// copy of the record's structured attributes.
type ErrorEvent struct {
	Time    time.Time
	Level   string
	Message string
	Attrs   map[string]any
}

// SetErrorSink registers fn to be invoked for every error-or-higher log
// record. Pass nil to disable. The sink is called inline on the logging
// goroutine, so it MUST return quickly and never block (buffer/spool and
// return). Safe to call at any time.
func SetErrorSink(fn func(ErrorEvent)) {
	if fn == nil {
		errorSink.Store(nil)
		return
	}
	errorSink.Store(&fn)
}

// hookHandler wraps a base slog.Handler and forwards error-level records to
// the registered sink after the base handler has written them.
type hookHandler struct {
	base slog.Handler
}

func (h *hookHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *hookHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.base.Handle(ctx, r)
	if r.Level >= slog.LevelError {
		if sink := errorSink.Load(); sink != nil {
			attrs := make(map[string]any, r.NumAttrs())
			r.Attrs(func(a slog.Attr) bool {
				attrs[a.Key] = a.Value.Any()
				return true
			})
			(*sink)(ErrorEvent{
				Time:    r.Time,
				Level:   r.Level.String(),
				Message: r.Message,
				Attrs:   attrs,
			})
		}
	}
	return err
}

func (h *hookHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &hookHandler{base: h.base.WithAttrs(as)}
}

func (h *hookHandler) WithGroup(name string) slog.Handler {
	return &hookHandler{base: h.base.WithGroup(name)}
}

// Init configures the package-level logger. Call once at startup.
//
// dataDir is the application data directory (logs will go in dataDir/logs).
// level may be "debug", "info", "warn", or "error" (case-insensitive).
func Init(dataDir, level string) error {
	logsDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return err
	}

	logFile := filepath.Join(logsDir, "tarang-"+time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	w := io.MultiWriter(os.Stdout, f)

	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	base := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			// Compact time format for human readability.
			if a.Key == slog.TimeKey {
				return slog.String("t", a.Value.Time().Format("15:04:05.000"))
			}
			return a
		},
	})
	// Wrap so error-level records can also be forwarded to a sink (the
	// crash/error reporter) once one is registered via SetErrorSink.
	h := &hookHandler{base: base}

	defaultMu.Lock()
	defaultLogger = slog.New(h)
	slog.SetDefault(defaultLogger)
	defaultMu.Unlock()

	return nil
}

// L returns the package-level logger. Falls back to slog.Default if Init has
// not yet been called.
func L() *slog.Logger {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultLogger == nil {
		return slog.Default()
	}
	return defaultLogger
}

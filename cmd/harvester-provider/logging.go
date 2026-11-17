package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
)

var diagnosticLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

func newDiagnosticLog(w io.Writer, level, format string, secrets []string) (*slog.Logger, error) {
	var minLevel slog.Level
	switch level {
	case "trace":
		minLevel = -8
	case "debug":
		minLevel = slog.LevelDebug
	case "", "info":
		minLevel = slog.LevelInfo
	case "warn":
		minLevel = slog.LevelWarn
	case "error":
		minLevel = slog.LevelError
	default:
		return nil, errors.New("invalid DEVSY_LOG_LEVEL")
	}
	pairs := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		for _, part := range append([]string{secret}, strings.Split(secret, "\n")...) {
			if len(part) >= 4 {
				pairs = append(pairs, part, "[REDACTED]")
			}
		}
	}
	redactor := strings.NewReplacer(pairs...)
	opts := &slog.HandlerOptions{Level: minLevel, ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		switch a.Key {
		case slog.TimeKey:
			a.Key = "ts"
			a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02T15:04:05.000Z0700"))
		case slog.LevelKey:
			a.Key = "level"
			level := a.Value.Any().(slog.Level)
			switch {
			case level < slog.LevelDebug:
				a.Value = slog.StringValue("trace")
			case level < slog.LevelInfo:
				a.Value = slog.StringValue("debug")
			case level < slog.LevelWarn:
				a.Value = slog.StringValue("info")
			case level < slog.LevelError:
				a.Value = slog.StringValue("warn")
			default:
				a.Value = slog.StringValue("error")
			}
		}
		if a.Value.Kind() == slog.KindString {
			a.Value = slog.StringValue(redactor.Replace(a.Value.String()))
		}
		return a
	}}
	var handler slog.Handler
	switch format {
	case "", "json":
		handler = slog.NewJSONHandler(w, opts)
	case "text", "plain":
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, errors.New("invalid DEVSY_LOG_FORMAT")
	}
	return slog.New(handler), nil
}

func configureDiagnostics(c config, operation string) error {
	logger, err := newDiagnosticLog(os.Stderr, env("DEVSY_LOG_LEVEL", "info"), env("DEVSY_LOG_FORMAT", "json"), []string{c.UserData, c.SSHFlags})
	if err != nil {
		return err
	}
	diagnosticLog = logger.With("component", "provider", "provider", "harvester", "operation", operation, "machine", c.VMName)
	return nil
}

func progress(message string, args ...any) {
	diagnosticLog.Info(message, args...)
}

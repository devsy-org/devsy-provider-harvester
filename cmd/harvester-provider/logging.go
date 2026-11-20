package main

import (
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/devsy-org/devsy/logging"
	"github.com/devsy-org/devsy/logging/redact"
)

var diagnosticLog = slog.New(slog.NewJSONHandler(io.Discard, nil))

func newDiagnosticLog(w io.Writer, level, format string, environment, secrets []string, attrs ...slog.Attr) (*slog.Logger, error) {
	config, invalid := logging.ResolveProvider(level, format)
	if invalid.Level {
		return nil, errors.New("invalid DEVSY_LOG_LEVEL")
	}
	if invalid.Format {
		return nil, errors.New("invalid DEVSY_LOG_FORMAT")
	}
	redactor := redact.New(redact.Options{
		Values:             secrets,
		Environment:        environment,
		Mask:               "[REDACTED]",
		MinExplicitBytes:   4,
		SplitExplicitLines: true,
	})
	return slog.New(logging.NewHandler(w, logging.HandlerOptions{
		Level:        config.Level,
		Format:       config.Format,
		Redactor:     redactor,
		TrustedAttrs: attrs,
	})), nil
}

func configureDiagnostics(c config, operation string) error {
	logger, err := newDiagnosticLog(os.Stderr, env("DEVSY_LOG_LEVEL", "info"), env("DEVSY_LOG_FORMAT", "json"), os.Environ(), []string{c.UserData, c.SSHFlags},
		slog.String("component", "provider"), slog.String("provider", "harvester"),
		slog.String("operation", operation), slog.String("machine", c.VMName))
	if err != nil {
		return err
	}
	diagnosticLog = logger
	return nil
}

func progress(message string, args ...any) {
	diagnosticLog.Info(message, args...)
}

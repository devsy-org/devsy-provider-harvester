package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticLevelsAndWireFormat(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  string
	}{
		{"", "info,warn,error"},
		{"trace", "trace,debug,info,warn,error"},
		{"debug", "debug,info,warn,error"},
		{"info", "info,warn,error"},
		{"warn", "warn,error"},
		{"error", "error"},
	} {
		t.Run(tc.level, func(t *testing.T) {
			var out bytes.Buffer
			logger, err := newDiagnosticLog(&out, tc.level, "json", []string{"canary-secret"})
			if err != nil {
				t.Fatal(err)
			}
			for _, level := range []slog.Level{-8, slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
				logger.Log(context.Background(), level, "diagnostic canary-secret", "attempt", 3, "detail", "useful canary-secret")
			}
			var levels []string
			dec := json.NewDecoder(&out)
			for dec.More() {
				var record map[string]any
				if err := dec.Decode(&record); err != nil {
					t.Fatal(err)
				}
				levels = append(levels, record["level"].(string))
				if _, err := time.Parse("2006-01-02T15:04:05.000Z0700", record["ts"].(string)); err != nil {
					t.Fatal(err)
				}
				if record["msg"] != "diagnostic [REDACTED]" || record["detail"] != "useful [REDACTED]" || record["attempt"] != float64(3) {
					t.Fatalf("unexpected record: %#v", record)
				}
			}
			if strings.Join(levels, ",") != tc.want {
				t.Fatalf("levels %v, want %s", levels, tc.want)
			}
		})
	}
}

func TestDiagnosticFormatsAndInvalidConfiguration(t *testing.T) {
	for _, format := range []string{"text", "plain"} {
		var out bytes.Buffer
		logger, err := newDiagnosticLog(&out, "", format, nil)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("visible")
		if !strings.Contains(out.String(), "msg=visible") {
			t.Fatalf("missing text diagnostic: %q", out.String())
		}
	}
	for _, tc := range []struct{ level, format string }{{"secret-invalid", "json"}, {"info", "secret-invalid"}} {
		_, err := newDiagnosticLog(&bytes.Buffer{}, tc.level, tc.format, nil)
		if err == nil || strings.Contains(err.Error(), "secret-invalid") {
			t.Fatalf("invalid configuration leaked or was accepted: %v", err)
		}
	}
}

func TestInvalidDiagnosticConfigDoesNotRunProvider(t *testing.T) {
	exe := lifecycleFixture(t)
	calls := t.TempDir() + "/calls"
	t.Setenv("HARVESTER_TEST_KUBECTL_CALLS", calls)
	t.Setenv("DEVSY_LOG_LEVEL", "secret-invalid")
	out, stderr, err := runLifecycleFixture(t, exe, "create")
	if err == nil || out != "" || !strings.Contains(stderr, "invalid DEVSY_LOG_LEVEL") || strings.Contains(stderr, "secret-invalid") {
		t.Fatalf("invalid configuration was not rejected safely: %q %q %v", out, stderr, err)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatalf("provider invoked kubectl before configuration validation: %v", err)
	}
}

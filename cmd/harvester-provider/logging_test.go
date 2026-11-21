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
		{"INFO", "info,warn,error"},
		{"warn", "warn,error"},
		{"error", "error"},
	} {
		t.Run(tc.level, func(t *testing.T) {
			var out bytes.Buffer
			logger, err := newDiagnosticLog(&out, tc.level, "json", nil, []string{"canary-secret"})
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
				if record["msg"] != "diagnostic [REDACTED]" || record["detail"] != "[REDACTED]" || record["attempt"] != float64(3) {
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
	for _, tc := range []struct {
		format string
		json   bool
	}{
		{"", true}, {"json", true}, {"JSON", true}, {"auto", true}, {"AuTo", true},
		{"text", false}, {"TeXt", false}, {"plain", false}, {"PLAIN", false},
		{"logfmt", false}, {"LoGfMt", false},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var out bytes.Buffer
			logger, err := newDiagnosticLog(&out, "InFo", tc.format, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			logger.Info("visible")
			if tc.json {
				var record map[string]any
				if err := json.Unmarshal(out.Bytes(), &record); err != nil || record["msg"] != "visible" {
					t.Fatalf("unexpected JSON diagnostic: %q (%v)", out.String(), err)
				}
			} else if !strings.Contains(out.String(), "msg=visible") {
				t.Fatalf("missing text diagnostic: %q", out.String())
			}
		})
	}
	for _, tc := range []struct{ level, format, want string }{
		{"secret-invalid", "json", "invalid DEVSY_LOG_LEVEL"},
		{" info", "json", "invalid DEVSY_LOG_LEVEL"},
		{"info ", "json", "invalid DEVSY_LOG_LEVEL"},
		{"info", "secret-invalid", "invalid DEVSY_LOG_FORMAT"},
		{"info", " json", "invalid DEVSY_LOG_FORMAT"},
		{"info", "logfmt ", "invalid DEVSY_LOG_FORMAT"},
		{"invalid", "invalid", "invalid DEVSY_LOG_LEVEL"},
	} {
		_, err := newDiagnosticLog(&bytes.Buffer{}, tc.level, tc.format, nil, nil)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("level %q format %q: got %v, want %q", tc.level, tc.format, err, tc.want)
		}
	}
}

func TestDiagnosticSecretsAndScope(t *testing.T) {
	var out bytes.Buffer
	logger, err := newDiagnosticLog(&out, "", "", []string{"HARVESTER_TOKEN=environment-canary"}, []string{"abc", "first-line\nsecond-line", "four"},
		slog.String("component", "provider"), slog.String("provider", "harvester"),
		slog.String("operation", "create"), slog.String("machine", "vm-first-line"))
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("abc first-line second-line four", "operation", "spoofed",
		"combined", "first-line\nsecond-line",
		"detail", "https://user:password@example.com Authorization: Bearer tokenvalue",
		"basic", "Authorization: Basic encodedvalue", "environment", "environment-canary")
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"component": "provider", "provider": "harvester", "operation": "create",
		"machine": "[REDACTED]", "msg": "abc [REDACTED] [REDACTED] [REDACTED]",
		"combined": "[REDACTED]", "basic": "Authorization: [REDACTED]", "environment": "[REDACTED]",
		"detail": "https://[REDACTED]@example.com Authorization: [REDACTED]",
	} {
		if record[key] != want {
			t.Errorf("%s = %v, want %q", key, record[key], want)
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

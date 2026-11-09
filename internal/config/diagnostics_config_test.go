package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestGetConfigLoadsServerCheckSettingsAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("diagnostics:\n  serverCheck:\n    enabled: false\n    delayMs: 300\n  rules:\n    interbase-syntax: off\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := GetConfig(path)
	if err != nil {
		t.Fatalf("GetConfig() error = %v, want valid server-check settings", err)
	}
	if got.Diagnostics.ServerCheckEnabled() {
		t.Fatal("ServerCheckEnabled() = true, want false from YAML")
	}
	if got.Diagnostics.ServerCheckDelay() != 300*time.Millisecond {
		t.Fatalf("ServerCheckDelay() = %s, want 300ms", got.Diagnostics.ServerCheckDelay())
	}
	if got.Diagnostics.Rules["interbase-syntax"] != "off" {
		t.Fatalf("interbase-syntax rule = %q, want off", got.Diagnostics.Rules["interbase-syntax"])
	}

	defaults := DiagnosticsConfig{}
	if !defaults.ServerCheckEnabled() {
		t.Fatal("zero-value ServerCheckEnabled() = false, want the default true")
	}
	if defaults.ServerCheckDelay() != 750*time.Millisecond {
		t.Fatalf("zero-value ServerCheckDelay() = %s, want 750ms", defaults.ServerCheckDelay())
	}
}

func TestDiagnosticsConfigRejectsServerCheckDelayBelowMinimum(t *testing.T) {
	cfg := &Config{Diagnostics: DiagnosticsConfig{ServerCheck: ServerCheckConfig{DelayMs: 50}}}
	err := cfg.Validate()
	if err == nil || err.Error() != "diagnostics.serverCheck.delayMs must be at least 100" {
		t.Fatalf("Validate() error = %v, want minimum-delay validation error", err)
	}
}

func diagnosticsTestdataPath(t *testing.T, fp string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "testdata", fp)
}

func TestGetConfigLoadsValidDiagnosticsRules(t *testing.T) {
	got, err := GetConfig(diagnosticsTestdataPath(t, "diagnostics_valid.yml"))
	if err != nil {
		t.Fatalf("GetConfig() error = %v, want nil for a valid diagnostics section", err)
	}
	want := map[string]string{
		"interbase-unknown-variable": "error",
		"interbase-null-comparison":  "warning",
		"interbase-ambiguous-column": "default",
	}
	if diff := cmp.Diff(want, got.Diagnostics.Rules); diff != "" {
		t.Fatalf("Diagnostics.Rules mismatch (-want +got):\n%s", diff)
	}
}

func TestGetConfigRejectsUnknownDiagnosticsRuleCode(t *testing.T) {
	_, err := GetConfig(diagnosticsTestdataPath(t, "diagnostics_unknown_code.yml"))
	if err == nil {
		t.Fatal("GetConfig() = nil error, want a validation error for an unknown rule code")
	}
}

func TestGetConfigRejectsUnknownDiagnosticsLevel(t *testing.T) {
	_, err := GetConfig(diagnosticsTestdataPath(t, "diagnostics_unknown_level.yml"))
	if err == nil {
		t.Fatal("GetConfig() = nil error, want a validation error for an unknown level")
	}
}

func TestConfigValidateAcceptsEmptyDiagnostics(t *testing.T) {
	if err := (&Config{}).Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a config with no diagnostics section", err)
	}
}

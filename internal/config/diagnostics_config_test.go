package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

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

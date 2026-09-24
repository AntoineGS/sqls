package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sourcegraph/jsonrpc2"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

func marshalConfigChangeParams(t *testing.T, cfg *config.Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{"settings": map[string]interface{}{"sqls": cfg}})
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(raw)
}

func TestCaptureEditorSnapshotDeepCopiesDiagnosticRules(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	const uri = "file:///policy-snapshot.sql"
	s.stateMu.Lock()
	s.WSCfg = &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	s.files[uri] = &File{Text: "SELECT 1"}
	s.stateMu.Unlock()

	snapshot, err := s.captureEditorSnapshot(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.DiagnosticOptions.Rules["interbase-null-comparison"]; got != "error" {
		t.Fatalf("DiagnosticOptions.Rules[interbase-null-comparison] = %q, want error", got)
	}

	s.stateMu.Lock()
	s.WSCfg.Diagnostics.Rules["interbase-null-comparison"] = "off"
	s.stateMu.Unlock()
	if got := snapshot.DiagnosticOptions.Rules["interbase-null-comparison"]; got != "error" {
		t.Fatalf("snapshot rules were not deep-copied: mutating the live config changed a captured snapshot to %q", got)
	}
}

func TestHandleWorkspaceDidChangeConfigurationIncrementsPolicyRevisionAndRequeues(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	s.stateMu.RLock()
	before := s.policyRevision
	s.stateMu.RUnlock()

	cfg := &config.Config{Diagnostics: config.DiagnosticsConfig{Rules: map[string]string{"interbase-null-comparison": "error"}}}
	raw := marshalConfigChangeParams(t, cfg)
	if _, err := s.handleWorkspaceDidChangeConfiguration(context.Background(), nil, &jsonrpc2.Request{Params: &raw}); err != nil {
		t.Fatal(err)
	}

	s.stateMu.RLock()
	after := s.policyRevision
	s.stateMu.RUnlock()
	if after != before+1 {
		t.Fatalf("policyRevision = %d, want %d", after, before+1)
	}

	s.diagnosticWorkMu.Lock()
	allOpen := s.diagnosticAllOpen
	s.diagnosticWorkMu.Unlock()
	if !allOpen {
		t.Fatal("workspace configuration change did not requeue every open document")
	}
}

func TestHandleWorkspaceDidChangeConfigurationIncrementsRevisionEvenWithoutDiagnosticsChange(t *testing.T) {
	s := NewServer()
	defer s.Stop()
	s.stateMu.RLock()
	before := s.policyRevision
	s.stateMu.RUnlock()

	raw := marshalConfigChangeParams(t, &config.Config{LowercaseKeywords: true})
	if _, err := s.handleWorkspaceDidChangeConfiguration(context.Background(), nil, &jsonrpc2.Request{Params: &raw}); err != nil {
		t.Fatal(err)
	}

	s.stateMu.RLock()
	after := s.policyRevision
	s.stateMu.RUnlock()
	if after != before+1 {
		t.Fatalf("policyRevision = %d, want %d (every configuration change advances the fence)", after, before+1)
	}
}

func TestDiagnosticsSnapshotCurrentRejectsStalePolicyRevision(t *testing.T) {
	tx := newDiagnosticsTestContext(t, dialect.DatabaseDriverInterBase)
	const uri = "file:///policy-revision.sql"
	tx.server.stateMu.Lock()
	tx.server.files[uri] = &File{Text: "SELECT 1", Version: 1}
	tx.server.stateMu.Unlock()

	snapshot, ok := tx.server.diagnosticsSnapshot(uri)
	if !ok {
		t.Fatal("could not snapshot open document")
	}
	if !tx.server.diagnosticsSnapshotCurrent(snapshot) {
		t.Fatal("a freshly captured snapshot should be current")
	}

	tx.server.stateMu.Lock()
	tx.server.policyRevision++
	tx.server.stateMu.Unlock()

	if tx.server.diagnosticsSnapshotCurrent(snapshot) {
		t.Fatal("a snapshot captured under an old policy revision must not stay current after a policy change")
	}
}

func TestDiagnosticsForSnapshotAppliesRuleOverrideSeverity(t *testing.T) {
	text := "SELECT ID FROM T WHERE V = NULL;"
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	got := diagnosticsForSnapshot(documentDiagnosticsSnapshot{
		uri: "file:///policy-severity.sql", text: text, variant: variant,
		diagnosticOptions: sqlsymbol.DiagnosticOptions{Rules: map[string]string{"interbase-null-comparison": "error"}},
	})
	if len(got) != 1 || diagnosticCode(got[0]) != "interbase-null-comparison" {
		t.Fatalf("diagnostics = %+v, want a single interbase-null-comparison finding", got)
	}
	if got[0].Severity != 1 {
		t.Fatalf("severity = %d, want 1 (error, per rule override)", got[0].Severity)
	}
}

func TestDiagnosticsForSnapshotOffRuleSuppressesFinding(t *testing.T) {
	text := "SELECT ID FROM T WHERE V = NULL;"
	variant := dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase}
	got := diagnosticsForSnapshot(documentDiagnosticsSnapshot{
		uri: "file:///policy-off.sql", text: text, variant: variant,
		diagnosticOptions: sqlsymbol.DiagnosticOptions{Rules: map[string]string{"interbase-null-comparison": "off"}},
	})
	if len(got) != 0 {
		t.Fatalf("diagnostics = %+v, want none with the rule turned off", got)
	}
}

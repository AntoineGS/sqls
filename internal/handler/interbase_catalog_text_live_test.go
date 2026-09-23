//go:build interbase && cgo && linux && amd64

package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/config"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
)

const interBaseLiveSingletonNeedle = "SELECT F_LEFT(config_value, 1) FROM branch_config WHERE config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' into :AllowNoPayments;"

func TestInterBaseLiveLegacyCatalogSingleton(t *testing.T) {
	configPath := os.Getenv("SQLS_LEGACY_CATALOG_CONFIG")
	charset := os.Getenv("SQLS_LEGACY_CATALOG_CHARSET")
	documentPath := os.Getenv("SQLS_LEGACY_CATALOG_DOCUMENT")
	if configPath == "" || charset == "" || documentPath == "" {
		t.Skip("requires SQLS_LEGACY_CATALOG_CONFIG, SQLS_LEGACY_CATALOG_CHARSET, and SQLS_LEGACY_CATALOG_DOCUMENT")
	}
	for name, path := range map[string]string{
		"SQLS_LEGACY_CATALOG_CONFIG":   configPath,
		"SQLS_LEGACY_CATALOG_DOCUMENT": documentPath,
	} {
		if !filepath.IsAbs(path) {
			t.Fatalf("%s must be an absolute path", name)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(charset), "WIN1252") {
		t.Fatalf("SQLS_LEGACY_CATALOG_CHARSET must be the owner-confirmed WIN1252")
	}

	loaded, err := config.GetConfig(configPath)
	if err != nil {
		// config.Load includes YAML source in some parse errors; never print it.
		t.Fatalf("load SQLS legacy catalog config failed (%T)", err)
	}
	if len(loaded.Connections) == 0 || loaded.Connections[0] == nil {
		t.Fatal("SQLS legacy catalog config has no first connection")
	}
	cfg := *loaded.Connections[0]
	if cfg.Driver != dialect.DatabaseDriverInterBase {
		t.Fatal("first configured connection is not InterBase")
	}
	if cfg.InterBase != nil {
		interBaseConfig := *cfg.InterBase
		cfg.InterBase = &interBaseConfig
	} else {
		cfg.InterBase = &database.InterBaseConfig{}
	}
	cfg.InterBase.CatalogTextCharset = strings.TrimSpace(charset)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := database.Open(&cfg)
	if err != nil {
		t.Fatalf("open configured InterBase connection failed (%T)", err)
	}
	defer conn.Close()
	repo := database.NewInterBaseDBRepositoryFromConnection(conn)
	updater := database.NewDBCacheUpdater(repo)
	cache, err := updater.GenerateDBCachePrimary(ctx)
	if err != nil {
		t.Fatalf("primary database cache failed (%T)", err)
	}
	columns, err := updater.GenerateDBCacheSecondary(ctx)
	if err != nil {
		t.Fatalf("secondary database cache failed (%T)", err)
	}
	cache.ColumnsWithParent = columns
	catalog, supported, err := updater.GenerateCatalogCache(ctx)
	if err != nil || !supported || catalog == nil {
		t.Fatalf("full catalog failed: supported=%v err-type=%T", supported, err)
	}
	cache.Catalog = catalog

	procedure, ok := catalog.Procedures["ACCOUNTINGPERIOD"]
	if !ok || procedure == nil || !procedure.Source.Valid {
		t.Fatal("ACCOUNTINGPERIOD procedure source is absent")
	}
	if !utf8.ValidString(procedure.Source.String) || strings.ContainsRune(procedure.Source.String, '\uFFFD') || !strings.Contains(procedure.Source.String, "CRÉATION") {
		t.Fatal("ACCOUNTINGPERIOD source failed UTF-8/CRÉATION validation")
	}
	t.Logf("full catalog loaded: %d procedures; ACCOUNTINGPERIOD source is valid UTF-8 and contains CRÉATION", len(catalog.Procedures))

	var matchingKeys [][]string
	for _, index := range catalog.Indexes {
		if index != nil && index.RelationName == "BRANCH_CONFIG" && index.Unique.Valid && index.Unique.Bool && index.Active.Valid && index.Active.Bool && !index.Expression.Valid {
			matchingKeys = append(matchingKeys, append([]string(nil), index.Columns...))
		}
	}
	wantedKey := []string{"BRANCHID", "CONFIG_NAME"}
	keyFound := false
	for _, key := range matchingKeys {
		if equalStrings(key, wantedKey) {
			keyFound = true
			break
		}
	}
	if !keyFound {
		t.Fatalf("BRANCH_CONFIG active unique key columns mismatch: found %q", matchingKeys)
	}
	t.Logf("BRANCH_CONFIG active unique key columns verified: %q", wantedKey)

	document, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatalf("read SQLS_LEGACY_CATALOG_DOCUMENT failed (%T)", err)
	}
	text := string(document)
	offset := strings.Index(text, interBaseLiveSingletonNeedle)
	if offset < 0 {
		t.Fatal("expected BRANCH_CONFIG reproduction is absent from the full document")
	}
	line := strings.Count(text[:offset], "\n")
	selectOffset := offset + strings.Index(interBaseLiveSingletonNeedle, "SELECT")
	lineStart := strings.LastIndex(text[:selectOffset], "\n") + 1
	character := utf16Units(text[lineStart:selectOffset])
	variant := conn.DriverVariant()
	if variant.Driver != dialect.DatabaseDriverInterBase {
		t.Fatal("opened connection does not report InterBase")
	}
	cacheSnapshot := snapshotDiagnosticCatalog(cache)
	found := diagnosticsForSnapshot(documentDiagnosticsSnapshot{text: text, variant: variant, cacheSnapshot: cacheSnapshot})
	if !hasSingletonDiagnosticAtPosition(found, line, character) {
		t.Fatalf("expected interbase-singleton-select diagnostic absent at zero-based line %d, character %d", line, character)
	}
	t.Logf("full-document interbase-singleton-select diagnostic verified at one-based line %d", line+1)

	corrected := strings.Replace(text, "config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS'", "config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' AND BRANCHID = '00'", 1)
	correctedFound := diagnosticsForSnapshot(documentDiagnosticsSnapshot{text: corrected, variant: variant, cacheSnapshot: cacheSnapshot})
	if hasSingletonDiagnosticAtPosition(correctedFound, line, character) {
		t.Fatalf("interbase-singleton-select diagnostic remained at target statement after adding BRANCHID predicate")
	}
	if !sameDiagnosticsExceptTargetSingleton(found, correctedFound, line, character) {
		t.Fatal("adding BRANCHID predicate changed unrelated full-document diagnostics")
	}
	t.Log("corrected full document no longer reports the target singleton warning; unrelated diagnostics preserved")
}

func hasSingletonDiagnosticAtPosition(diagnostics []lsp.Diagnostic, line, character int) bool {
	for _, diagnostic := range diagnostics {
		if diagnosticCode(diagnostic) == "interbase-singleton-select" && int(diagnostic.Range.Start.Line) == line && int(diagnostic.Range.Start.Character) == character {
			return true
		}
	}
	return false
}

func sameDiagnosticsExceptTargetSingleton(before, after []lsp.Diagnostic, line, character int) bool {
	filtered := func(diagnostics []lsp.Diagnostic) []lsp.Diagnostic {
		result := make([]lsp.Diagnostic, 0, len(diagnostics))
		for _, diagnostic := range diagnostics {
			if diagnosticCode(diagnostic) == "interbase-singleton-select" && int(diagnostic.Range.Start.Line) == line && int(diagnostic.Range.Start.Character) == character {
				continue
			}
			result = append(result, diagnostic)
		}
		return result
	}
	left, right := filtered(before), filtered(after)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Range != right[i].Range || diagnosticCode(left[i]) != diagnosticCode(right[i]) || left[i].Message != right[i].Message || left[i].Severity != right[i].Severity {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

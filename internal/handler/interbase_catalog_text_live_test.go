//go:build interbase && cgo && linux && amd64

package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

var errCatalogSnapshotUnavailable = errors.New("catalog snapshot unavailable")

type catalogSnapshotObservation struct {
	outcome          string
	elapsed          time.Duration
	errType          string
	deadlineExceeded bool
	canceled         bool
	contextErr       string
}

// catalogSnapshotPrivacyForwarder preserves the repository capabilities and
// delegates successful snapshot reads unchanged. On snapshot error it returns
// a fixed sentinel so database.cache's fallback log cannot disclose raw driver
// error text.
type catalogSnapshotPrivacyForwarder struct {
	database.DBRepository
	database.CatalogRepository
	snapshotSource database.CatalogSnapshotRepository
	phase          string
	observations   map[string]catalogSnapshotObservation
	logf           func(string, ...any)
}

func newCatalogSnapshotPrivacyForwarder(repo database.DBRepository, catalog database.CatalogRepository, snapshot database.CatalogSnapshotRepository, phase string, logf func(string, ...any)) *catalogSnapshotPrivacyForwarder {
	return &catalogSnapshotPrivacyForwarder{
		DBRepository: repo, CatalogRepository: catalog, snapshotSource: snapshot,
		phase: phase, observations: make(map[string]catalogSnapshotObservation), logf: logf,
	}
}

func (f *catalogSnapshotPrivacyForwarder) setPhase(phase string) { f.phase = phase }

func (f *catalogSnapshotPrivacyForwarder) record(phase string) catalogSnapshotObservation {
	return f.observations[phase]
}

func (f *catalogSnapshotPrivacyForwarder) CatalogSnapshot(ctx context.Context) (database.DBRepository, func() error, error) {
	started := time.Now()
	repo, closeSnapshot, err := f.snapshotSource.CatalogSnapshot(ctx)
	outcome := "fallback"
	if err == nil && repo != nil {
		outcome = "used"
	}
	observation := catalogSnapshotObservation{
		outcome: outcome, elapsed: time.Since(started), errType: fmt.Sprintf("%T", err),
		deadlineExceeded: errors.Is(err, context.DeadlineExceeded),
		canceled:         errors.Is(err, context.Canceled), contextErr: liveContextError(ctx),
	}
	f.observations[f.phase] = observation
	if f.logf != nil {
		f.logf("phase=%s snapshot=%s elapsed=%s err_type=%s is_deadline_exceeded=%t is_canceled=%t ctx_err=%s",
			f.phase, observation.outcome, observation.elapsed, observation.errType,
			observation.deadlineExceeded, observation.canceled, observation.contextErr)
	}
	if err != nil {
		return repo, closeSnapshot, errCatalogSnapshotUnavailable
	}
	return repo, closeSnapshot, nil
}

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

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	totalStarted := time.Now()
	defer func() {
		t.Logf("live acceptance total elapsed=%s deadline_remaining=%s ctx_err=%s", time.Since(totalStarted), liveDeadlineRemaining(ctx), liveContextError(ctx))
	}()

	phaseStarted := time.Now()
	conn, err := database.Open(&cfg)
	logLivePhase(t, ctx, "open", phaseStarted, err)
	if err != nil {
		t.Fatalf("open configured InterBase connection failed (%T)", err)
	}
	defer conn.Close()
	repo := database.NewInterBaseDBRepositoryFromConnection(conn)
	catalogRepo, ok := repo.(database.CatalogRepository)
	if !ok {
		t.Fatal("InterBase repository does not provide catalog capability")
	}
	snapshotRepo, ok := repo.(database.CatalogSnapshotRepository)
	if !ok {
		t.Fatal("InterBase repository does not provide catalog snapshot capability")
	}
	forwarder := newCatalogSnapshotPrivacyForwarder(repo, catalogRepo, snapshotRepo, "", func(format string, args ...any) {
		t.Logf(format, args...)
	})
	updater := database.NewDBCacheUpdater(forwarder)
	phaseStarted = time.Now()
	forwarder.setPhase("primary-cache")
	cache, err := updater.GenerateDBCachePrimary(ctx)
	logLivePhase(t, ctx, "primary-cache", phaseStarted, err)
	if err != nil {
		t.Fatalf("primary database cache failed (%T)", err)
	}
	phaseStarted = time.Now()
	forwarder.setPhase("secondary-cache")
	columns, err := updater.GenerateDBCacheSecondary(ctx)
	logLivePhase(t, ctx, "secondary-cache", phaseStarted, err)
	if err != nil {
		t.Fatalf("secondary database cache failed (%T)", err)
	}
	cache.ColumnsWithParent = columns
	phaseStarted = time.Now()
	forwarder.setPhase("full-catalog")
	catalog, supported, err := updater.GenerateCatalogCache(ctx)
	logLivePhase(t, ctx, "full-catalog", phaseStarted, err)
	if err != nil || !supported || catalog == nil {
		t.Fatalf("full catalog failed: supported=%v err-type=%T", supported, err)
	}
	if snapshot := forwarder.record("full-catalog"); snapshot.outcome != "used" {
		t.Fatalf("full catalog completed without using snapshot capability: outcome=%s err-type=%s", snapshot.outcome, snapshot.errType)
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
	expectedRange := lsp.Range{
		Start: lsp.Position{Line: line, Character: utf16Units(text[lineStart:selectOffset])},
		End:   lsp.Position{Line: line, Character: utf16Units(text[lineStart : selectOffset+len("SELECT")])},
	}
	variant := conn.DriverVariant()
	if variant.Driver != dialect.DatabaseDriverInterBase {
		t.Fatal("opened connection does not report InterBase")
	}
	cacheSnapshot := snapshotDiagnosticCatalog(cache)
	found := diagnosticsForSnapshot(documentDiagnosticsSnapshot{text: text, variant: variant, cacheSnapshot: cacheSnapshot})
	if !hasSingletonDiagnosticAtRange(found, expectedRange) {
		t.Fatalf("expected interbase-singleton-select diagnostic absent at range %+v", expectedRange)
	}
	t.Logf("full-document interbase-singleton-select diagnostic verified at one-based line %d", line+1)

	corrected := strings.Replace(text, "config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS'", "config_name = 'WEB_IMPORT_ALLOW_NO_PAYMENTS' AND BRANCHID = '00'", 1)
	correctedFound := diagnosticsForSnapshot(documentDiagnosticsSnapshot{text: corrected, variant: variant, cacheSnapshot: cacheSnapshot})
	if hasSingletonDiagnosticAtRange(correctedFound, expectedRange) {
		t.Fatalf("interbase-singleton-select diagnostic remained at target statement after adding BRANCHID predicate")
	}
	if !sameDiagnosticsExceptTargetSingleton(found, correctedFound, expectedRange) {
		t.Fatal("adding BRANCHID predicate changed unrelated full-document diagnostics")
	}
	t.Log("corrected full document no longer reports the target singleton warning; unrelated diagnostics preserved")
}

func hasSingletonDiagnosticAtRange(diagnostics []lsp.Diagnostic, expected lsp.Range) bool {
	for _, diagnostic := range diagnostics {
		if diagnosticCode(diagnostic) == "interbase-singleton-select" && diagnostic.Range == expected {
			return true
		}
	}
	return false
}

func logLivePhase(t *testing.T, ctx context.Context, phase string, started time.Time, err error) {
	t.Helper()
	t.Logf("phase=%s elapsed=%s deadline_remaining=%s err_type=%T is_deadline_exceeded=%t is_canceled=%t ctx_err=%s",
		phase, time.Since(started), liveDeadlineRemaining(ctx), err,
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), liveContextError(ctx))
}

func liveDeadlineRemaining(ctx context.Context) string {
	deadline, ok := ctx.Deadline()
	if !ok {
		return "none"
	}
	return time.Until(deadline).String()
}

func liveContextError(ctx context.Context) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return context.DeadlineExceeded.Error()
	case errors.Is(ctx.Err(), context.Canceled):
		return context.Canceled.Error()
	default:
		return "nil"
	}
}

func sameDiagnosticsExceptTargetSingleton(before, after []lsp.Diagnostic, expected lsp.Range) bool {
	filtered := func(diagnostics []lsp.Diagnostic) []lsp.Diagnostic {
		result := make([]lsp.Diagnostic, 0, len(diagnostics))
		for _, diagnostic := range diagnostics {
			if diagnosticCode(diagnostic) == "interbase-singleton-select" && diagnostic.Range == expected {
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

func TestCatalogSnapshotPrivacyForwarderPreservesSnapshotAndCatalogCapabilities(t *testing.T) {
	wantRepo := &fakeSnapshotDBRepository{}
	closed := false
	var viewsCalled bool
	catalogRepo := &fakeSnapshotCatalogRepository{describeViews: func(context.Context) ([]*database.ViewDesc, error) {
		viewsCalled = true
		return []*database.ViewDesc{{Name: "SAFE_VIEW"}}, nil
	}}
	source := &fakeSnapshotSource{
		CatalogRepository: catalogRepo,
		snapshot: func(context.Context) (database.DBRepository, func() error, error) {
			return wantRepo, func() error { closed = true; return nil }, nil
		},
	}
	var logged bytes.Buffer
	forwarder := newCatalogSnapshotPrivacyForwarder(source, source.CatalogRepository, source, "full-catalog", func(format string, args ...any) {
		fmt.Fprintf(&logged, format, args...)
	})

	var catalog database.CatalogRepository = forwarder
	views, err := catalog.DescribeViews(context.Background())
	if err != nil || !viewsCalled || len(views) != 1 || views[0].Name != "SAFE_VIEW" {
		t.Fatalf("embedded catalog capability not preserved: views=%v called=%t err-type=%T", views, viewsCalled, err)
	}

	gotRepo, closeSnapshot, err := forwarder.CatalogSnapshot(context.Background())
	if err != nil || gotRepo != wantRepo || closeSnapshot == nil {
		t.Fatalf("successful snapshot was not forwarded unchanged: repo=%T err-type=%T", gotRepo, err)
	}
	if err := closeSnapshot(); err != nil || !closed {
		t.Fatalf("snapshot closer not preserved: closed=%t err-type=%T", closed, err)
	}
	if got := forwarder.record("full-catalog"); got.outcome != "used" || got.errType != "<nil>" {
		t.Fatalf("snapshot observation = %+v, want used/nil", got)
	}
	if !strings.Contains(logged.String(), "snapshot=used") {
		t.Fatalf("safe snapshot status missing from log: %q", logged.String())
	}
}

func TestCatalogSnapshotPrivacyForwarderSanitizesFallbackAndPreservesCloser(t *testing.T) {
	const secretMarker = "test-only-sensitive-backend-detail"
	originalErr := &fakeSnapshotError{message: secretMarker, cause: context.DeadlineExceeded}
	closed := false
	source := &fakeSnapshotSource{snapshot: func(context.Context) (database.DBRepository, func() error, error) {
		return nil, func() error { closed = true; return nil }, originalErr
	}}
	var logged bytes.Buffer
	forwarder := newCatalogSnapshotPrivacyForwarder(source, source.CatalogRepository, source, "primary-cache", func(format string, args ...any) {
		fmt.Fprintf(&logged, format, args...)
	})

	gotRepo, closeSnapshot, err := forwarder.CatalogSnapshot(context.Background())
	if gotRepo != nil || closeSnapshot == nil || err != errCatalogSnapshotUnavailable {
		t.Fatalf("fallback result = (%T, closer=%t, err-type=%T), want nil/preserved closer/fixed sentinel", gotRepo, closeSnapshot != nil, err)
	}
	if errors.Is(err, originalErr) || strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("fallback error retained original details: %q", err.Error())
	}
	if strings.Contains(logged.String(), secretMarker) {
		t.Fatalf("fallback log leaked original error message: %q", logged.String())
	}
	if !errors.Is(originalErr, context.DeadlineExceeded) || !strings.Contains(logged.String(), "is_deadline_exceeded=true") {
		t.Fatalf("safe fallback log lacks deadline evidence: %q", logged.String())
	}
	if got := forwarder.record("primary-cache"); got.outcome != "fallback" || got.errType != "*handler.fakeSnapshotError" || !got.deadlineExceeded || got.contextErr != "nil" {
		t.Fatalf("fallback observation = %+v", got)
	}
	if err := closeSnapshot(); err != nil || !closed {
		t.Fatalf("fallback closer not preserved: closed=%t err-type=%T", closed, err)
	}
}

func TestCatalogSnapshotPrivacyForwarderRecordsSilentNilFallback(t *testing.T) {
	source := &fakeSnapshotSource{snapshot: func(context.Context) (database.DBRepository, func() error, error) {
		return nil, nil, nil
	}}
	forwarder := newCatalogSnapshotPrivacyForwarder(source, source.CatalogRepository, source, "secondary-cache", nil)
	repo, closer, err := forwarder.CatalogSnapshot(context.Background())
	if repo != nil || closer != nil || err != nil {
		t.Fatalf("nil snapshot fallback = (%T, %t, %T), want nil/nil/nil", repo, closer != nil, err)
	}
	if got := forwarder.record("secondary-cache"); got.outcome != "fallback" || got.errType != "<nil>" {
		t.Fatalf("silent fallback observation = %+v", got)
	}
}

type fakeSnapshotDBRepository struct{ database.DBRepository }

type fakeSnapshotSource struct {
	database.DBRepository
	database.CatalogRepository
	snapshot func(context.Context) (database.DBRepository, func() error, error)
}

func (f *fakeSnapshotSource) CatalogSnapshot(ctx context.Context) (database.DBRepository, func() error, error) {
	return f.snapshot(ctx)
}

type fakeSnapshotCatalogRepository struct {
	describeViews func(context.Context) ([]*database.ViewDesc, error)
}

func (f *fakeSnapshotCatalogRepository) DescribeViews(ctx context.Context) ([]*database.ViewDesc, error) {
	return f.describeViews(ctx)
}

func (*fakeSnapshotCatalogRepository) DescribeProcedures(context.Context) ([]*database.ProcedureDesc, error) {
	return nil, nil
}

func (*fakeSnapshotCatalogRepository) DescribeGenerators(context.Context) ([]*database.GeneratorDesc, error) {
	return nil, nil
}

func (*fakeSnapshotCatalogRepository) DescribeTriggers(context.Context) ([]*database.TriggerDesc, error) {
	return nil, nil
}

func (*fakeSnapshotCatalogRepository) DescribeDomains(context.Context) ([]*database.DomainDesc, error) {
	return nil, nil
}

func (*fakeSnapshotCatalogRepository) DescribeIndexes(context.Context) ([]*database.IndexDesc, error) {
	return nil, nil
}

func (*fakeSnapshotCatalogRepository) DescribeFunctions(context.Context) ([]*database.FunctionDesc, error) {
	return nil, nil
}

type fakeSnapshotError struct {
	message string
	cause   error
}

func (e *fakeSnapshotError) Error() string { return e.message }

func (e *fakeSnapshotError) Unwrap() error { return e.cause }

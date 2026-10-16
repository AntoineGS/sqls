package language

import (
	"sync"
	"testing"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
)

func TestDocumentAnalysisCacheIdentity(t *testing.T) {
	base := Request{
		Document: Document{URI: "file:///cache.sql", Text: "SELECT ID FROM T", Version: 1, Binding: 7},
		Context:  Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	identityChanges := []struct {
		name   string
		mutate func(*Request)
	}{
		{"URI", func(req *Request) { req.Document.URI = "file:///other.sql" }},
		{"version", func(req *Request) { req.Document.Version++ }},
		{"exact text", func(req *Request) { req.Document.Text += " WHERE ID = 1" }},
		{"binding", func(req *Request) { req.Document.Binding++ }},
		{"dialect variant", func(req *Request) { req.Context.Variant.Variant = dialect.SQLVariantInterBase1 }},
	}
	for _, test := range identityChanges {
		t.Run(test.name, func(t *testing.T) {
			service := NewService()
			baselineRequest := base
			baselineRequest.Document.CacheEpoch = service.CacheEpoch()
			baseline := service.documentAnalysis(baselineRequest)
			changed := baselineRequest
			test.mutate(&changed)
			if got := service.documentAnalysis(changed); got == baseline {
				t.Fatalf("changed %s reused the current baseline entry", test.name)
			}
		})
	}
}

func TestDocumentAnalysisCacheReusesAndInvalidates(t *testing.T) {
	service := NewService()
	request := Request{
		Document: Document{URI: "file:///cache.sql", Text: "SELECT ID FROM T", Version: 1, Binding: 7, CacheEpoch: service.CacheEpoch()},
		Context:  Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	first := service.documentAnalysis(request)
	if got := service.documentAnalysis(request); got != first {
		t.Fatal("same document identity returned a different cache entry")
	}
	parsed, err := first.parsed()
	if err != nil {
		t.Fatal(err)
	}
	parsedAgain, err := service.documentAnalysis(request).parsed()
	if err != nil {
		t.Fatal(err)
	}
	if parsed == nil || parsedAgain != parsed {
		t.Fatal("same document identity did not reuse its parsed tree")
	}

	service.InvalidateDocument(request.Document.URI)
	request.Document.CacheEpoch = service.CacheEpoch()
	if got := service.documentAnalysis(request); got == first {
		t.Fatal("invalidated document returned its old cache entry")
	}
}

func TestDocumentAnalysisRejectsStaleInsertAfterInvalidation(t *testing.T) {
	service := NewService()
	oldRequest := Request{
		Document: Document{URI: "file:///cache.sql", Text: "SELECT ID FROM T", Version: 1, Binding: 7, CacheEpoch: service.CacheEpoch()},
		Context:  Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	oldEntry := service.documentAnalysis(oldRequest)
	if got := service.documentAnalysis(oldRequest); got != oldEntry {
		t.Fatal("same version did not retain its current entry")
	}
	staleRequest := oldRequest // captured now, but paused before documentAnalysis
	service.InvalidateDocument(oldRequest.Document.URI)
	newRequest := oldRequest
	newRequest.Document.Version = 2
	newRequest.Document.Text = "SELECT NAME FROM T"
	newRequest.Document.CacheEpoch = service.CacheEpoch()
	newEntry := service.documentAnalysis(newRequest)
	if got := service.documentAnalysis(staleRequest); got == newEntry {
		t.Fatal("stale request reused the newer entry")
	}
	if got := service.documentAnalysis(newRequest); got != newEntry {
		t.Fatal("stale request replaced the newer version")
	}

	closedService := NewService()
	closedRequest := oldRequest
	closedRequest.Document.CacheEpoch = closedService.CacheEpoch()
	closedEntry := closedService.documentAnalysis(closedRequest)
	staleClosedRequest := closedRequest // captured before close, lookup is delayed
	closedService.InvalidateDocument(closedRequest.Document.URI)
	staleEntry := closedService.documentAnalysis(staleClosedRequest)
	reopenedRequest := closedRequest
	reopenedRequest.Document.CacheEpoch = closedService.CacheEpoch()
	reopenedEntry := closedService.documentAnalysis(reopenedRequest)
	if staleEntry == closedEntry || reopenedEntry == staleEntry || reopenedEntry == closedEntry {
		t.Fatal("request captured before close repopulated the document cache")
	}
	if got := closedService.documentAnalysis(reopenedRequest); got != reopenedEntry {
		t.Fatal("current reopened document did not retain its cache entry")
	}

	olderSameEpoch := newRequest
	olderSameEpoch.Document.Version = 1
	olderSameEpoch.Document.Text = "SELECT ID FROM T"
	if got := service.documentAnalysis(olderSameEpoch); got == newEntry {
		t.Fatal("older version unexpectedly reused the newer request entry")
	}
	if got := service.documentAnalysis(newRequest); got != newEntry {
		t.Fatal("older version replaced the newer cache entry")
	}
}

func TestDocumentAnalysisCachesStrictAndTolerantAnalyses(t *testing.T) {
	service := NewService()
	request := Request{
		Document: Document{URI: "file:///procedure.sql", Text: "CREATE PROCEDURE P AS DECLARE VARIABLE V; BEGIN END;", Version: 1, CacheEpoch: service.CacheEpoch()},
		Context:  Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	entry := service.documentAnalysis(request)
	if _, err := entry.analysis(); err == nil {
		t.Fatal("strict analysis succeeded for an incomplete procedure")
	}
	first, err := entry.diagnosticAnalysis()
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("tolerant diagnostic analysis returned nil")
	}
	second, err := entry.diagnosticAnalysis()
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("tolerant diagnostic analysis was not cached")
	}
}

func TestDocumentAnalysisConcurrentReaders(t *testing.T) {
	service := NewService()
	request := Request{
		Document: Document{URI: "file:///cache.sql", Text: "SELECT ID FROM T", Version: 1, CacheEpoch: service.CacheEpoch()},
		Context:  Context{Variant: dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3}},
	}
	entry := service.documentAnalysis(request)

	type accessResult struct {
		parsed         ast.TokenList
		strict         *sqlsymbol.Analysis
		diagnostics    *sqlsymbol.Analysis
		parseErr       error
		strictErr      error
		diagnosticsErr error
	}
	const readers = 16
	results := make(chan accessResult, readers)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			parsed, parseErr := entry.parsed()
			strict, strictErr := entry.analysis()
			diagnostics, diagnosticsErr := entry.diagnosticAnalysis()
			results <- accessResult{
				parsed:         parsed,
				strict:         strict,
				diagnostics:    diagnostics,
				parseErr:       parseErr,
				strictErr:      strictErr,
				diagnosticsErr: diagnosticsErr,
			}
		}()
	}
	wg.Wait()
	close(results)

	var first accessResult
	i := 0
	for result := range results {
		if result.parseErr != nil || result.strictErr != nil || result.diagnosticsErr != nil {
			t.Errorf("reader %d accessor errors: parse=%v strict=%v diagnostics=%v", i, result.parseErr, result.strictErr, result.diagnosticsErr)
		}
		if result.parsed == nil || result.strict == nil || result.diagnostics == nil {
			t.Errorf("reader %d returned nil cached syntax/analysis", i)
		}
		if i == 0 {
			first = result
		} else if result.parsed != first.parsed || result.strict != first.strict || result.diagnostics != first.diagnostics {
			t.Errorf("reader %d did not reuse the same cached syntax and analyses", i)
		}
		i++
	}
}

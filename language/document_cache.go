package language

import (
	"sync"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/sqlsymbol"
	"github.com/sqls-server/sqls/parser"
)

// documentAnalysis holds syntax-derived data for one document identity.
// The tree and analyses are shared read-only between language requests.
type documentAnalysis struct {
	uri     string
	version int64
	binding uint64
	variant dialect.DriverVariant
	text    string

	parseOnce sync.Once
	tree      ast.TokenList
	parseErr  error

	analysisOnce sync.Once
	strict       *sqlsymbol.Analysis
	strictErr    error

	diagnosticOnce sync.Once
	diagnostics    *sqlsymbol.Analysis
	diagnosticErr  error
}

func newDocumentAnalysis(req Request) *documentAnalysis {
	return &documentAnalysis{
		uri:     req.Document.URI,
		version: req.Document.Version,
		binding: req.Document.Binding,
		variant: req.Context.Variant,
		text:    req.Document.Text,
	}
}

// documentAnalysis returns the cached syntax entry for req when its identity
// and insertion epoch are current. Empty URIs and stale snapshots get a
// detached entry that cannot affect the service cache.
func (s *Service) documentAnalysis(req Request) *documentAnalysis {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()

	if req.Document.URI == "" || req.Document.CacheEpoch != s.cacheEpoch {
		return newDocumentAnalysis(req)
	}
	if s.documents == nil {
		s.documents = make(map[string]*documentAnalysis)
	}
	if current := s.documents[req.Document.URI]; current != nil {
		if current.matches(req) {
			return current
		}
		if current.binding == req.Document.Binding && req.Document.Version < current.version {
			return newDocumentAnalysis(req)
		}
	}
	entry := newDocumentAnalysis(req)
	s.documents[req.Document.URI] = entry
	return entry
}

func (d *documentAnalysis) matches(req Request) bool {
	return d.uri == req.Document.URI &&
		d.version == req.Document.Version &&
		d.binding == req.Document.Binding &&
		d.text == req.Document.Text &&
		d.variant == req.Context.Variant
}

func (d *documentAnalysis) parsed() (ast.TokenList, error) {
	d.parseOnce.Do(func() {
		d.tree, d.parseErr = parser.ParseWithDriverVariant(d.text, d.variant)
	})
	return d.tree, d.parseErr
}

func (d *documentAnalysis) analysis() (*sqlsymbol.Analysis, error) {
	d.analysisOnce.Do(func() {
		d.strict, d.strictErr = sqlsymbol.Analyze(d.text, d.variant)
	})
	return d.strict, d.strictErr
}

func (d *documentAnalysis) diagnosticAnalysis() (*sqlsymbol.Analysis, error) {
	d.diagnosticOnce.Do(func() {
		d.diagnostics, d.diagnosticErr = sqlsymbol.AnalyzeDiagnostics(d.text, d.variant)
	})
	return d.diagnostics, d.diagnosticErr
}

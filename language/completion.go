package language

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/sqls-server/sqls/ast"
	"github.com/sqls-server/sqls/ast/astutil"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/parser/parseutil"
	"github.com/sqls-server/sqls/token"
)

// Completion item kinds. Values match the LSP CompletionItemKind
// enumeration exactly so a host can cast language.CompletionItem.Kind to its
// own transport type without a translation table.
const (
	KindText          CompletionKind = 1
	KindMethod        CompletionKind = 2
	KindFunction      CompletionKind = 3
	KindConstructor   CompletionKind = 4
	KindField         CompletionKind = 5
	KindVariable      CompletionKind = 6
	KindClass         CompletionKind = 7
	KindInterface     CompletionKind = 8
	KindModule        CompletionKind = 9
	KindProperty      CompletionKind = 10
	KindUnit          CompletionKind = 11
	KindValue         CompletionKind = 12
	KindEnum          CompletionKind = 13
	KindKeyword       CompletionKind = 14
	KindSnippet       CompletionKind = 15
	KindColor         CompletionKind = 16
	KindFile          CompletionKind = 17
	KindReference     CompletionKind = 18
	KindFolder        CompletionKind = 19
	KindEnumMember    CompletionKind = 20
	KindConstant      CompletionKind = 21
	KindStruct        CompletionKind = 22
	KindEvent         CompletionKind = 23
	KindOperator      CompletionKind = 24
	KindTypeParameter CompletionKind = 25
)

// Service implements the language algorithms (completion, hover,
// signature help, diagnostics, format and symbols) over an explicit
// Context supplied per call. It retains only the latest syntax analysis for
// each document URI; results and metadata remain request-scoped.
type Service struct {
	documentMu sync.Mutex
	documents  map[string]*documentAnalysis
	cacheEpoch uint64
}

// NewService returns a Service with an empty per-document syntax cache.
func NewService() *Service {
	return &Service{documents: make(map[string]*documentAnalysis), cacheEpoch: 0}
}

// CacheEpoch returns the current invalidation epoch. Hosts capture it with
// their editor snapshot and copy it into subsequent language requests.
func (s *Service) CacheEpoch() uint64 {
	s.documentMu.Lock()
	defer s.documentMu.Unlock()
	return s.cacheEpoch
}

// InvalidateDocument advances the insertion fence and drops the URI's latest
// entry. Requests captured before this call may finish, but cannot repopulate
// or replace an entry after it.
func (s *Service) InvalidateDocument(uri string) {
	s.documentMu.Lock()
	s.cacheEpoch++
	delete(s.documents, uri)
	s.documentMu.Unlock()
}

func stampFor(req Request) Stamp {
	stamp := Stamp{
		URI:         req.Document.URI,
		Version:     req.Document.Version,
		Binding:     req.Document.Binding,
		Incarnation: req.Context.Incarnation,
		Key:         req.Context.Key,
	}
	if req.Context.Snapshot != nil {
		stamp.Generation = req.Context.Snapshot.Generation
		stamp.Revision = req.Context.Snapshot.Revision
	}
	return stamp
}

// completionRelevantMetadata is the set of categories that can contribute
// catalog completion candidates. A failed/blocked/cancelled category is
// terminal: it must not keep a client re-requesting completion forever
// once progressive loading has degraded.
var completionRelevantMetadata = []catalog.MetadataKind{
	catalog.MetadataSchemas, catalog.MetadataRelations,
	catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll,
	catalog.MetadataViews, catalog.MetadataProcedures,
	catalog.MetadataGenerators, catalog.MetadataFunctions,
}

// CompletionIncomplete reports whether snapshot's own metadata state means
// a completion result computed against it should be treated as
// Incomplete: true. It only judges the metadata snapshot itself; a host
// with additional connection-lifecycle facts (for example "still
// attaching", which has no representation in a catalog.MetadataSnapshot)
// ORs its own such facts into the result this returns.
func CompletionIncomplete(snapshot *catalog.MetadataSnapshot) bool {
	if snapshot == nil {
		return false
	}
	if !snapshot.Started && !snapshot.StartFailed {
		return true
	}
	for _, kind := range completionRelevantMetadata {
		state := snapshot.Status[kind].State
		if state == catalog.MetadataPending || state == catalog.MetadataLoading {
			return true
		}
	}
	return false
}

// Complete returns completion candidates for position in req.Document,
// interpreted against req.Context. A nil req.Context.Snapshot (or one
// with a nil Cache) yields language-only candidates: keywords and
// functions, with no catalog knowledge.
func (s *Service) Complete(ctx context.Context, req Request, position Position) (Result[CompletionList], error) {
	if err := ctx.Err(); err != nil {
		return Result[CompletionList]{}, ErrCanceled
	}

	var cache *catalog.Cache
	if req.Context.Snapshot != nil {
		cache = req.Context.Snapshot.Cache
	}
	eng := &completionEngine{cache: cache, driver: req.Context.Variant.Driver, variant: req.Context.Variant.Variant}

	text := req.Document.Text
	offset, err := ByteOffset(text, position)
	if err != nil {
		return Result[CompletionList]{}, err
	}
	parsed, err := s.documentAnalysis(req).parsed()
	if err != nil {
		return Result[CompletionList]{}, err
	}

	parserPos, err := parserPositionAt(text, offset)
	if err != nil {
		return Result[CompletionList]{}, err
	}
	pos := token.Pos{Line: parserPos.Line, Col: parserPos.UTF16Column}

	nodeWalker := parseutil.NewNodeWalker(parsed, pos)
	completionCtx := getCompletionTypes(nodeWalker)

	definedTables, err := parseutil.ExtractTable(parsed, pos)
	if err != nil {
		return Result[CompletionList]{}, err
	}
	definedSubQueries, err := parseutil.ExtractSubQueryViews(parsed, pos)
	if err != nil {
		return Result[CompletionList]{}, err
	}

	lastWord := getLastWordWithVariant(text, position.Line+1, position.UTF16Column, eng.driverVariant())
	withBackQuote := strings.HasPrefix(lastWord, "`")

	var items []CompletionItem

	if eng.cache != nil {
		if completionTypeIs(completionCtx.types, completionTypeColumn) {
			candidates := eng.columnCandidates(definedTables, completionCtx.parent)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(completionCtx.types, completionTypeReferencedTable) {
			candidates := eng.referencedTableCandidates(definedTables)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(completionCtx.types, completionTypeTable) {
			excl := definedTables
			if completionTypeIs(completionCtx.types, completionTypeJoin) {
				excl = nil
			}
			candidates := eng.tableCandidates(completionCtx.parent, excl)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
			// An InterBase selectable procedure is legal wherever a relation
			// is. Views are deliberately NOT added here: they are already in
			// SchemaTables, so tableCandidates has offered them once.
			items = append(items, eng.selectableProcedureCandidates()...)
		}
		if completionTypeIs(completionCtx.types, completionTypeProcedureName) {
			items = append(items, eng.procedureCandidates()...)
		}
		if completionTypeIs(completionCtx.types, completionTypeView) && !completionTypeIs(completionCtx.types, completionTypeTable) {
			items = append(items, eng.viewCandidates(completionCtx.parent)...)
		}
		if insideGenIDCall(nodeWalker) {
			items = append(items, eng.generatorCandidates()...)
		}
		if completionTypeIs(completionCtx.types, completionTypeSchema) {
			candidates := eng.schemaCandidates()
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(completionCtx.types, completionTypeSubQuery) {
			candidates := eng.subQueryCandidates(definedSubQueries)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		if completionTypeIs(completionCtx.types, completionTypeSubQueryColumn) {
			candidates := eng.subQueryColumnCandidates(definedSubQueries)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates)
			}
			items = append(items, candidates...)
		}
		joinOn := completionTypeIs(completionCtx.types, completionTypeJoinOn)
		if completionTypeIs(completionCtx.types, completionTypeJoin) || joinOn {
			table, err := parseutil.ExtractLastTable(parsed, pos)
			if err != nil {
				return Result[CompletionList]{}, err
			}
			tables, err := parseutil.ExtractPrevTables(parsed, pos)
			if err != nil {
				return Result[CompletionList]{}, err
			}
			candidates := eng.joinCandidates(table, tables, definedTables, joinOn, req.Context.LowercaseKeywords)
			if withBackQuote {
				candidates = toQuotedCandidates(candidates) // what to do here?
			}
			items = append(candidates, items...)
		}
	}

	if completionTypeIs(completionCtx.types, completionTypeKeyword) {
		drivers := dialect.DataBaseKeywordsForVariant(eng.driverVariant())
		items = append(items, eng.keywordCandidates(req.Context.LowercaseKeywords, drivers)...)
	}
	if completionTypeIs(completionCtx.types, completionTypeFunction) {
		drivers := dialect.DataBaseFunctionsForVariant(eng.driverVariant())
		items = append(items, eng.functionCandidates(req.Context.LowercaseKeywords, drivers)...)
		// A UDF is callable exactly where a built-in function is, so it needs
		// no new completion type and no new context.
		items = append(items, eng.externalFunctionCandidates()...)
	}

	items = filterCandidates(items, lastWord)
	populateSortText(items)

	var incomplete bool
	if req.Context.Snapshot != nil {
		incomplete = CompletionIncomplete(req.Context.Snapshot)
	}

	return Result[CompletionList]{
		Value: CompletionList{Items: items, Incomplete: incomplete},
		Stamp: stampFor(req),
	}, nil
}

// completionEngine holds the catalog and dialect inputs one Complete call
// generates candidates against. It is a request-scoped value, never
// retained across calls.
type completionEngine struct {
	cache   *catalog.Cache
	driver  dialect.DatabaseDriver
	variant dialect.SQLVariant
}

func (c *completionEngine) driverVariant() dialect.DriverVariant {
	return dialect.DriverVariant{Driver: c.driver, Variant: c.variant}
}

// catalogEnabled gates every InterBase-specific candidate generator.
// HasCatalog() is false both on a non-InterBase connection and in the
// window before the worker's secondary catalog pass lands, and in both
// cases completion must degrade silently to the driver-neutral behaviour.
func (c *completionEngine) catalogEnabled() bool {
	return c.driver == dialect.DatabaseDriverInterBase && c.cache.HasCatalog()
}

type completionType int

const (
	_ completionType = iota
	completionTypeKeyword
	completionTypeFunction
	completionTypeColumn
	completionTypeTable
	completionTypeReferencedTable
	completionTypeView
	completionTypeProcedureName
	completionTypeSubQuery
	completionTypeSubQueryColumn
	completionTypeChange
	completionTypeUser
	completionTypeSchema
	completionTypeJoin
	completionTypeJoinOn
)

func (ct completionType) String() string {
	switch ct {
	case completionTypeKeyword:
		return "Keyword"
	case completionTypeFunction:
		return "Function"
	case completionTypeColumn:
		return "Column"
	case completionTypeTable:
		return "Table"
	case completionTypeReferencedTable:
		return "ReferencedTable"
	case completionTypeView:
		return "View"
	case completionTypeProcedureName:
		return "ProcedureName"
	case completionTypeChange:
		return "Change"
	case completionTypeUser:
		return "User"
	case completionTypeSchema:
		return "Schema"
	case completionTypeSubQuery:
		return "SubQuery"
	case completionTypeSubQueryColumn:
		return "SubQueryColumn"
	case completionTypeJoin:
		return "Join clause"
	case completionTypeJoinOn:
		return "Join On condition"
	default:
		return ""
	}
}

func completionTypeIs(completionTypes []completionType, expect completionType) bool {
	for _, t := range completionTypes {
		if t == expect {
			return true
		}
	}
	return false
}

// Override the sort text for each completion item.
func populateSortText(items []CompletionItem) {
	for i := range items {
		items[i].SortText = getSortTextPrefix(items[i].Kind) + items[i].Label
	}
}

// Some completion kinds are more relevant than others.
// This prefix defines the alphabetic priority of each kind.
func getSortTextPrefix(kind CompletionKind) string {
	switch kind {
	case KindSnippet:
		return "00"
	case KindField:
		return "0"
	case KindClass:
		return "1"
	case KindModule:
		return "2"
	case KindFunction:
		return "10"
	case KindMethod:
		// Procedures, beside the external functions they resemble.
		return "10"
	case KindValue:
		// Generators, just below procedures and functions.
		return "11"
	case
		KindColor,
		KindConstant,
		KindConstructor,
		KindEnum,
		KindEnumMember,
		KindEvent,
		KindFile,
		KindFolder,
		KindInterface,
		KindKeyword,
		KindOperator,
		KindProperty,
		KindReference,
		KindStruct,
		KindText,
		KindTypeParameter,
		KindUnit,
		KindVariable:
		return "9999"
	default:
		return "9999"
	}
}

type completionParentType int

const (
	_ completionParentType = iota
	completionParentTypeNone
	completionParentTypeSchema
	completionParentTypeTable
	completionParentTypeSubQuery
)

type completionParent struct {
	Type completionParentType
	Name string
}

var noneCompletionParent = &completionParent{Type: completionParentTypeNone}

type completionContext struct {
	types  []completionType
	parent *completionParent
}

func getCompletionTypes(nw *parseutil.NodeWalker) *completionContext {
	memberIdentifierMatcher := astutil.NodeMatcher{
		NodeTypes: []ast.NodeType{ast.TypeMemberIdentifier},
	}

	syntaxPos := parseutil.CheckSyntaxPosition(nw)
	var t []completionType
	p := noneCompletionParent
	switch {
	case syntaxPos == parseutil.ColName:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				completionTypeColumn,
				completionTypeSubQueryColumn,
				completionTypeView,
			}
			p = &completionParent{
				Type: completionParentTypeTable,
				Name: mi.Parent.String(),
			}
		} else {
			t = []completionType{
				completionTypeColumn,
				completionTypeTable,
				completionTypeReferencedTable,
				completionTypeSubQueryColumn,
				completionTypeSubQuery,
				completionTypeView,
				completionTypeFunction,
			}
			p = noneCompletionParent
		}
	case syntaxPos == parseutil.AliasName:
		// pass
	case syntaxPos == parseutil.SelectExpr || syntaxPos == parseutil.CaseValue:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				completionTypeColumn,
				completionTypeView,
				completionTypeSubQueryColumn,
			}
			p = &completionParent{
				Type: completionParentTypeTable,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				completionTypeColumn,
				completionTypeTable,
				completionTypeReferencedTable,
				completionTypeView,
				completionTypeSubQueryColumn,
				completionTypeSubQuery,
				completionTypeFunction,
			}
		}
	case syntaxPos == parseutil.TableReference:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				completionTypeTable,
				completionTypeView,
				completionTypeSubQueryColumn,
			}
			p = &completionParent{
				Type: completionParentTypeSchema,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				completionTypeTable,
				completionTypeReferencedTable,
				completionTypeSchema,
				completionTypeView,
				completionTypeSubQuery,
			}
		}
	case syntaxPos == parseutil.WhereCondition:
		if nw.CurNodeIs(memberIdentifierMatcher) {
			// has parent
			mi := nw.CurNodeTopMatched(memberIdentifierMatcher).(*ast.MemberIdentifier)
			t = []completionType{
				completionTypeColumn,
				completionTypeView,
				completionTypeSubQueryColumn,
			}
			p = &completionParent{
				Type: completionParentTypeTable,
				Name: mi.ParentTok.NoQuoteString(),
			}
		} else {
			t = []completionType{
				completionTypeColumn,
				completionTypeTable,
				completionTypeReferencedTable,
				completionTypeView,
				completionTypeSubQueryColumn,
				completionTypeSubQuery,
				completionTypeFunction,
			}
		}
	case syntaxPos == parseutil.JoinClause:
		t = []completionType{
			completionTypeJoin,
			completionTypeTable,
			completionTypeReferencedTable,
			completionTypeSchema,
			completionTypeView,
			completionTypeSubQuery,
		}
	case syntaxPos == parseutil.JoinOn:
		t = []completionType{
			completionTypeJoinOn,
			completionTypeColumn,
			completionTypeReferencedTable,
			completionTypeSubQueryColumn,
			completionTypeSubQuery,
		}
	case syntaxPos == parseutil.InsertColumn:
		t = []completionType{
			completionTypeColumn,
			completionTypeView,
		}
	case syntaxPos == parseutil.ExecuteProcedure:
		// completionTypeKeyword is retained deliberately. multiKeywordMap is
		// dialect-independent, so PostgreSQL's legacy
		// "CREATE TRIGGER ... EXECUTE PROCEDURE f()" reaches this branch too;
		// without the keywords a PostgreSQL user would receive nothing at all,
		// because no procedure cache exists for that driver.
		t = []completionType{
			completionTypeProcedureName,
			completionTypeKeyword,
		}
	default:
		t = []completionType{
			completionTypeKeyword,
		}
	}
	return &completionContext{
		types:  t,
		parent: p,
	}
}

func filterCandidates(candidates []CompletionItem, lastWord string) []CompletionItem {
	filtered := []CompletionItem{}
	for _, candidate := range candidates {
		if strings.HasPrefix(strings.ToUpper(candidate.Label), strings.ToUpper(lastWord)) {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func getLine(text string, line int) string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	i := 1
	for scanner.Scan() {
		if i == line {
			return scanner.Text()
		}
		i++
	}
	return ""
}

func getLastWord(text string, line, char int) string {
	return getLastWordWithDriver(text, line, char, "")
}

// getLastWordWithDriver returns the word before the cursor using a driver's
// default variant. It is retained with its exact signature; see
// getLastWordWithVariant.
func getLastWordWithDriver(text string, line, char int, driver dialect.DatabaseDriver) string {
	return getLastWordWithVariant(text, line, char, dialect.DriverVariant{Driver: driver})
}

func getLastWordWithVariant(text string, line, char int, dv dialect.DriverVariant) string {
	t := getBeforeCursorText(text, line, char)
	s := getLine(t, line)

	wordPattern := "[\\w`]+$"
	if dv.Driver == dialect.DatabaseDriverInterBase {
		wordPattern = "[\\w$`]+$"
	}
	reg := regexp.MustCompile(wordPattern)
	ss := reg.FindAllString(s, -1)
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}

func getBeforeCursorText(text string, line, char int) string {
	writer := bytes.NewBufferString("")
	scanner := bufio.NewScanner(strings.NewReader(text))

	i := 1
	for scanner.Scan() {
		if i == line {
			t := scanner.Text()
			writer.WriteString(t[:byteOffsetForUTF16(t, char)])
			break
		}
		fmt.Fprintln(writer, scanner.Text())
		i++
	}
	return writer.String()
}

// byteOffsetForUTF16 converts a UTF-16 character offset to a byte offset in
// s, clamping to the end of the string.
func byteOffsetForUTF16(s string, char int) int {
	utf16Pos := 0
	for bytePos, r := range s {
		if utf16Pos >= char {
			return bytePos
		}
		utf16Pos++
		if r > 0xFFFF {
			utf16Pos++
		}
	}
	return len(s)
}

func toQuotedCandidates(candidates []CompletionItem) []CompletionItem {
	quotedCandidates := make([]CompletionItem, len(candidates))
	for i, candidate := range candidates {
		candidate.Label = fmt.Sprintf("`%s`", candidate.Label)
		quotedCandidates[i] = candidate
	}
	return quotedCandidates
}

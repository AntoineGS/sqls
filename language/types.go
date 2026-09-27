// Package language defines the transport-neutral value types, position
// conversions and staleness semantics behind SQLs's language algorithms
// (completion, hover, diagnostics, navigation, and so on) so the same
// implementation can be called from the standalone LSP server and from an
// in-process Go embedder. It depends only on catalog, dialect and
// metadata's public packages, the standard library, and never on any SQLs
// internal package, LSP transport type or host-specific DTO.
package language

import (
	"context"
	"errors"

	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/metadata"
)

// Errors returned by the language package's public API.
var (
	// ErrPosition reports that a Position or byte offset does not address
	// a valid location in the given text.
	ErrPosition = errors.New("language: invalid position")
	// ErrCanceled reports that the caller's context ended before a request
	// completed.
	ErrCanceled = errors.New("language: request canceled")
	// ErrStale reports that a request's document or metadata context is no
	// longer current for the store it targets.
	ErrStale = errors.New("language: stale document or metadata")
	// ErrInvalidDocument reports that a Document is malformed for the
	// operation requested (for example an empty URI).
	ErrInvalidDocument = errors.New("language: invalid document")
)

// Position is a zero-based line and UTF-16 code unit column, matching the
// LSP Position coordinate system without depending on any LSP package.
type Position struct {
	Line        int
	UTF16Column int
}

// Range is a span from Start up to and excluding End.
type Range struct {
	Start Position
	End   Position
}

// TextEdit replaces the text spanned by Range with Text.
type TextEdit struct {
	Range Range
	Text  string
}

// Document is one host's editor buffer: its URI, current text, an opaque
// host-assigned edit Version, and Binding. A host advances Binding whenever
// the document is rebound to a different database target, independently of
// Version or Text, so a stale-target result can be detected even when the
// document's own text and edit count are unchanged (see Current).
type Document struct {
	URI     string
	Text    string
	Version int64
	Binding uint64
}

// DetailReader loads one object's rendered detail. *metadata.Lease
// satisfies this interface directly.
type DetailReader interface {
	Detail(context.Context, metadata.Request) (metadata.Detail, error)
}

// Context is the explicit database/metadata context a host supplies for one
// request. There is no global current connection in the language package:
// every call carries its own Context. A nil Snapshot means language-only
// analysis with no catalog claims — completion, diagnostics and the rest
// run without object knowledge instead of blocking or erroring.
type Context struct {
	Key               metadata.Key
	Incarnation       uint64
	Snapshot          *catalog.MetadataSnapshot
	Variant           dialect.DriverVariant
	DialectResolved   bool
	LowercaseKeywords bool
	Details           DetailReader
}

// Request pairs a Document with the Context it should be interpreted
// against.
type Request struct {
	Document Document
	Context  Context
	Policy   DiagnosticPolicy
}

// DiagnosticPolicy configures rule-level diagnostics. The caller owns Rules
// and must not mutate it while a request is running.
type DiagnosticPolicy struct {
	Rules map[string]string
}

// Stamp identifies the exact document and metadata state a Result was
// computed against, so a host can reject a late-arriving Result once the
// document or store has moved on; see Current.
type Stamp struct {
	URI         string
	Version     int64
	Binding     uint64
	Incarnation uint64
	Generation  uint64
	Revision    uint64
	Key         metadata.Key
}

// Result pairs a value with the Stamp it was computed against.
type Result[T any] struct {
	Value T
	Stamp Stamp
}

// CompletionKind classifies a CompletionItem for client-side icon/sorting
// purposes. Its values match the LSP CompletionItemKind enumeration
// exactly so a host can cast a CompletionKind to its own transport type
// without a translation table; it is a distinct named type from SymbolKind
// so a value from one enumeration can never be passed to an API expecting
// the other, even though their numeric ranges overlap.
type CompletionKind int

// CompletionItem is one completion candidate.
type CompletionItem struct {
	Label           string
	Detail          string
	Documentation   string
	InsertText      string
	FilterText      string
	SortText        string
	Kind            CompletionKind
	Edit            *TextEdit
	AdditionalEdits []TextEdit
	Snippet         bool
}

// CompletionList is a completion response. Incomplete reports that further
// typing should re-request completion rather than filter this list
// client-side, for example because metadata is still loading.
type CompletionList struct {
	Items      []CompletionItem
	Incomplete bool
}

// Hover is a hover response. Range is nil when no token-bounded range
// applies.
type Hover struct {
	Markdown string
	Range    *Range
}

// Parameter is one signature parameter.
type Parameter struct {
	Label         string
	Documentation string
}

// Signature is one call signature.
type Signature struct {
	Label         string
	Documentation string
	Parameters    []Parameter
}

// SignatureHelp is a signature-help response.
type SignatureHelp struct {
	Signatures      []Signature
	ActiveSignature int
	ActiveParameter int
}

// Diagnostic is one document diagnostic.
type Diagnostic struct {
	Range    Range
	Severity int
	Code     string
	Message  string
	Source   string
}

// FormatOptions controls document formatting.
type FormatOptions struct {
	TabSize           int
	InsertSpaces      bool
	UppercaseKeywords bool
}

// Source is database-resident source text for a navigated object: its
// exact identity, the source text itself, and Provenance describing what
// kind of text it is (for example native source, reconstructed DDL, or an
// informational description) — never an on-disk path.
type Source struct {
	Object     catalog.ObjectID
	Text       string
	Provenance string
	// Note explains a non-executable fallback such as native catalog source
	// selected because DDL was unavailable, or an informational description.
	// It is empty for executable DDL.
	Note string
}

// Location is a navigation target. Source is non-nil when the location
// resolves into database-resident source rather than a position in the
// requesting document.
type Location struct {
	URI    string
	Range  Range
	Source *Source
}

// SymbolKind classifies a DocumentSymbol for client-side icon purposes.
// Its values match the LSP SymbolKind enumeration; it is a distinct named
// type from CompletionKind so a value from one enumeration can never be
// passed to an API expecting the other, even though their numeric ranges
// overlap (SymbolKindVariable and CompletionKind's KindEnum both equal 13).
type SymbolKind int

// DocumentSymbol is one entry in a document's symbol outline.
type DocumentSymbol struct {
	Name           string
	Kind           SymbolKind
	Range          Range
	SelectionRange Range
	Children       []DocumentSymbol
}

// RenameShape describes the host-facing WorkspaceEdit container a rename
// algorithm historically produced. It keeps transport policy out of the
// semantic algorithm while allowing adapters to preserve wire compatibility.
type RenameShape uint8

const (
	// RenameChanges maps document URIs directly to edits.
	RenameChanges RenameShape = iota
	// RenameDocumentChanges uses versioned TextDocumentEdit entries.
	RenameDocumentChanges
)

// RenameEdit is the edit set for one document produced by a rename.
type RenameEdit struct {
	URI   string
	Edits []TextEdit
	Shape RenameShape
}

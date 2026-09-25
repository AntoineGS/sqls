# Public language core

`language` holds the transport-neutral value types, position semantics and
staleness comparison behind SQLs's language algorithms (completion, hover,
diagnostics, formatting, navigation, symbols, rename), so the exact same
implementation can run inside the standalone LSP server and inside a future
in-process Go embedder such as MDExplorer's workbench, against one shared
metadata store rather than two.

It depends only on `catalog`, `dialect`, `metadata` and the standard
library. It never imports `internal/...`, an LSP transport package, or a
host-specific DTO type — see `language/public_api_test.go`, which
constructs `language.Request`/`language.Context` using only those public
packages as a compile-time proof.

## Positions

LSP addresses text with a zero-based `{Line, Character}` pair counted in
UTF-16 code units, not bytes or Unicode code points. `language.Position`
(`{Line, UTF16Column}`) is that same coordinate system without depending on
any LSP package:

```go
offset, err := language.ByteOffset(text, language.Position{Line: 3, UTF16Column: 12})
pos, err := language.PositionAt(text, offset)
```

- A rune above U+FFFF (for example most emoji) is two UTF-16 code units
  wide, not one; every other rune is one unit.
- `\n`, `\r\n` and a lone `\r` are each exactly one line delimiter — CRLF
  never counts as two lines or double-advances a column.
- Both conversions reject a malformed position or offset with `ErrPosition`
  instead of clamping into a neighboring token: a negative line/column, a
  line or column past the end of the text, an offset that does not fall on
  a UTF-8 rune boundary, and a `UTF16Column`/offset that lands on the low
  half of a surrogate pair (or inside a CRLF pair) are all rejected.

This intentionally reuses the column-unit boundary the existing SQL
tokenizer already targets (`token.Tokenizer`'s `Col`, which the
`// A tab is a single character in LSP position terms.` comment and its
one-Line-advance-per-CRLF handling document as UTF-16-oriented), rather than
inventing a second convention. See the task report for the one place the
tokenizer's own counting falls short of true UTF-16 width for supplementary-
plane runes inside identifiers and quoted strings, and why that gap is out
of scope for this package.

Native SQL execution may still encode text as CP1252 for InterBase; that is
a separate, later concern for the execution path. Editor text and positions
in this package are always Unicode, never a driver's wire encoding.

## Context and staleness

There is no global current connection in this package. Every call carries
an explicit `language.Context`: the metadata `Key`, the store's
`Incarnation`, an immutable `*catalog.MetadataSnapshot` (nil means
language-only analysis with no catalog claims), the resolved dialect
`Variant`, and a `Details` reader for on-demand object detail — satisfied
directly by `*metadata.Lease`.

A `language.Stamp` records exactly which document/metadata state a
`language.Result[T]` was computed against. `language.Current` reports
whether that stamp is still coherent with the document and context a host
holds now:

```go
if !language.Current(result.Stamp, currentDocument, currentContext) {
    // discard: the document was rebound or the store advanced since this
    // result was computed.
}
```

A document's `Binding` — separate from its edit `Version` — lets a host
detect "same text and version, different database" (a document rebound to
another target with no local edit) as well as the more familiar "same
document, edited since" and "same document, metadata refreshed" cases.

## What this task does not include

`language.Service` and its `Complete`/`Hover`/`Definition`/... methods are
introduced with their real implementations in later tasks (cache-only
operations, then navigation). This package currently exports only the value
types, `ByteOffset`, `PositionAt` and `Current` — no placeholder or
panic-on-call stub is shipped for `Service` while its behavior does not
exist yet.

## Minimal external-consumer shape

A future embedder builds a request with nothing but public packages:

```go
req := language.Request{
    Document: language.Document{URI: "memory:one", Text: "SELECT * FROM T", Version: 1, Binding: 1},
    Context: language.Context{
        Key:      metadata.Key{Engine: "interbase", URL: dsn},
        Snapshot: lease.Snapshot(),
        Variant:  dialect.DriverVariant{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase3},
        Details:  lease, // *metadata.Lease satisfies language.DetailReader
    },
}
```

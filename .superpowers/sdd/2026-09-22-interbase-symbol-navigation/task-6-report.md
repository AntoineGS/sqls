# Task 6 report

## Implementation summary

- Added `Analysis.Rename` with request-local symbol ownership checks, one-token
  InterBase identifier validation, reserved syntax-word rejection, decoded-name
  collision checks, rename blocking, and sorted declaration/use edits.
- Added handler-local InterBase routing through `localRename`; local edits use
  `WorkspaceEdit.Changes` and UTF-16/source-span adapters, while outside-
  procedure and non-InterBase requests retain the legacy fallback.
- Preserved optional colon spelling by replacing only identifier spans; SQL
  targets, ambiguous locals, callable/other-procedure targets, invalid names,
  collisions, nil symbols, and foreign symbols cannot be renamed.

## Changed files

`internal/sqlsymbol/rename.go`, `internal/sqlsymbol/rename_test.go`,
`internal/handler/rename.go`, `internal/handler/rename_test.go`.

## RED

Command:

```text
go test ./internal/sqlsymbol ./internal/handler -run 'TestRename|TestLocalRename' -count=1
```

Observed before implementation:

```text
internal/sqlsymbol/rename_test.go:30:18: a.Rename undefined (type *Analysis has no field or method Rename)
FAIL github.com/sqls-server/sqls/internal/sqlsymbol [build failed]
ok   github.com/sqls-server/sqls/internal/handler 0.010s
FAIL
```

## GREEN

```text
$ go test ./internal/sqlsymbol ./internal/handler -run 'TestRename|TestLocalRename' -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.003s
ok   github.com/sqls-server/sqls/internal/handler 0.011s

$ go test ./internal/sqlsymbol ./internal/handler -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.005s
ok   github.com/sqls-server/sqls/internal/handler 1.079s

$ go test ./... -count=1
all packages passed; handler 1.087s, sqlsymbol 0.005s

$ git diff --check
(no output; exit 0)

## Fix round 4: audit words outside the vendor appendix

### Finding and source check

Audited the ten words previously rejected before the reserved set was reconciled:
`CROSS`, `CURRENT_USER`, `LAST`, `LEADING`, `PARAMETER`, `SAVEPOINT`, `SQL`,
`START`, `SUBSTRING`, and `TRAILING`.

Source used: the local InterBase 2020 Language Reference Guide at
`/home/a.simard@multidev.local/.cache/opencode/tmp/interbase/Doc/LangRef.pdf`,
converted for inspection to `/tmp/opencode-interbase-langref.txt`. The guide's
“Keywords” introduction (printed page 164) says its table lists words reserved
from use in SQL programs and isql, includes DSQL/isql/gpre keywords, and that
these keywords cannot occur in user-declared identifiers unless delimited. The
following “InterBase Keywords” section (printed page 165 onward) specifically
defines its appendix entries as reserved in all dialects. None of these ten
words appears in that appendix's 293-word list, which is enforced by
`TestInterBaseReservedWordsMatchVendorAppendix`.

`SAVEPOINT` is a clear case of contextual SQL syntax rather than a reserved
identifier: section 9.88 (printed page 114) uses it in statement grammar and
explicitly says “A savepoint name can be any valid SQL identifier.” For the
other nine, the guide provides no exception to its reserved-word appendix; an
appearance as a grammar term, expression/built-in name, or completion entry is
not evidence that an identifier is forbidden. No source evidence was found
that any of these ten is unsafe as a procedure-local identifier. Therefore no
new syntax-specific exclusions were added; the exact vendor reserved set
remains intact.

### Changes

- Added `TestRenameAllowsNonReservedInterBaseSyntaxWords`, which asserts that
  renaming to each of the ten words is accepted. The comment records the source
  distinction and the direct `SAVEPOINT` evidence. Existing vendor-set tests
  continue to reject actual appendix words.
- No production code change: evidence does not justify treating these
  contextual/non-reserved words as forbidden local identifiers.

### RED / GREEN

No production behavior was changed, so there is no meaningful pre-fix RED
state. The regression test documents the already-correct behavior; it fails if
any of these names is added to the reserved-word rejection set.

Focused check:

```text
$ go test ./dialect ./internal/sqlsymbol ./internal/handler -run 'TestInterBaseReserved|TestRename|TestLocalRename' -count=1
ok   github.com/sqls-server/sqls/dialect 0.002s
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.004s
ok   github.com/sqls-server/sqls/internal/handler 0.010s
```

### Self-review / concerns

- This audit preserves the appendix's explicit authoritative reserved-word
  boundary rather than adding exclusions based on broad SQL completion or
  token inventories.
- No InterBase server was available to execute dynamic DDL acceptance probes;
  decisions are grounded in the vendor's identifier conventions and keyword
  appendix, with an explicit `SAVEPOINT` identifier example. If server-level
  evidence later contradicts the guide, revise the policy with a targeted
  syntax constraint and server-backed regression.
```

## Self-review

- Pure analysis emits byte-span edits and never imports LSP; handler conversion
  preserves UTF-16 positions and excludes prefix colons.
- Same-scope semantic keys catch quoted/unquoted collisions while allowing
  self/case-only unquoted renames; generic completion functions such as `ABS`
  are not treated as reserved syntax words.
- JSON-RPC coverage confirms a changed nonzero-version document returns current
  in-memory local edits without a version-zero `DocumentChanges` result.

## Concerns

Dialect 1 double-quoted strings remain non-identifiers by the existing lexer;
quoted identifier replacement is therefore accepted only where that variant
recognizes delimited identifiers (Dialect 3).

## Fix round 1

### Changes

- Added `dialect.IsInterBaseReservedWord`, backed by the authoritative
  InterBase syntax keyword list in `dialect/interbase.go`; it remains separate
  from broad cross-dialect completion/function inventories and handles the
  Dialect 3-only `TIME`/`TIMESTAMP` words.
- Added decoded-empty quoted-name rejection (`""`) after token validation.
- Expanded end-to-end rename coverage for the HEADEREMPLYID_TEMP fixture,
  mixed-case uses, nested and separate procedures, input/output parameters,
  comments and trivia in replacement input, Dialect 1 quoted rejection,
  quoted/unquoted semantic collisions, and handled ambiguous handler targets.

### RED

Command:

```text
go test ./internal/sqlsymbol ./internal/handler -run 'TestRename|TestLocalRename' -count=1
```

Observed before the fix:

```text
--- FAIL: TestRenameRejectsInterBaseReservedWordsCaseInsensitively
    Rename("AS") succeeded, want reserved-word validation error
    Rename("as") succeeded, want reserved-word validation error
    Rename("BEGIN") succeeded, want reserved-word validation error
    Rename("begin") succeeded, want reserved-word validation error
    Rename("CHAR") succeeded, want reserved-word validation error
    Rename("char") succeeded, want reserved-word validation error
--- FAIL: TestRenameRejectsEmptyDecodedQuotedName
    Rename("") succeeded, want empty decoded-name validation error
FAIL
```

### GREEN

```text
$ go test ./internal/sqlsymbol ./internal/handler -run 'TestRename|TestLocalRename' -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.003s
ok   github.com/sqls-server/sqls/internal/handler 0.009s

$ go test ./internal/sqlsymbol ./internal/handler -count=1
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.005s
ok   github.com/sqls-server/sqls/internal/handler 1.074s
```

### Fix self-review

- Reserved validation now shares the dialect package's InterBase source of
  truth and is case-insensitive; quoted names bypass reserved-word rejection
  as required by delimited-identifier semantics.
- The applied-SQL tests verify only the selected procedure's declaration and
  local uses change, including nested blocks, while columns, strings,
  comments, and equal names in another procedure remain unchanged.

## Fix round 2

### Changes and source

- Replaced the rename check's reuse of `interbaseKeywords` with a dedicated
  `interbaseReservedWords` set. The completion-oriented list remains separate.
- The dedicated list is based on the vendor's InterBase 2020 Language Reference
  Guide, Appendix “InterBase Keywords”, which states those words are reserved
  in all dialects: https://docwiki.embarcadero.com/docs/products/interbase/2020/LangRef.pdf
- Added `EXTRACT`, `TYPE`, `WEEKDAY`, and `YEARDAY`; Dialect 3-only `TIME` and
  `TIMESTAMP` handling remains variant-specific. Quoted names remain exempt.
- Added dialect-level and end-to-end rename tests for omitted words in upper
  and lower case, plus valid non-reserved function-like `ABS` and `DATEADD`.

### RED

Command:

```text
go test ./dialect ./internal/sqlsymbol ./internal/handler -run 'TestInterBaseReserved|TestRename|TestLocalRename' -count=1
```

Observed before the fix:

```text
--- FAIL: TestInterBaseReservedWordsUseDedicatedVendorSet
    IsInterBaseReservedWord("EXTRACT") = false, want true
    IsInterBaseReservedWord("extract") = false, want true
    IsInterBaseReservedWord("TYPE") = false, want true
    IsInterBaseReservedWord("type") = false, want true
    IsInterBaseReservedWord("WEEKDAY") = false, want true
    IsInterBaseReservedWord("weekday") = false, want true
    IsInterBaseReservedWord("YEARDAY") = false, want true
    IsInterBaseReservedWord("yearday") = false, want true
--- FAIL: TestRenameRejectsVendorReservedWordsCaseInsensitively
    Rename("EXTRACT") succeeded, want vendor reserved-word validation error
    Rename("extract") succeeded, want vendor reserved-word validation error
    Rename("TYPE") succeeded, want vendor reserved-word validation error
    Rename("type") succeeded, want vendor reserved-word validation error
    Rename("WEEKDAY") succeeded, want vendor reserved-word validation error
    Rename("weekday") succeeded, want vendor reserved-word validation error
    Rename("YEARDAY") succeeded, want vendor reserved-word validation error
    Rename("yearday") succeeded, want vendor reserved-word validation error
FAIL
```

### GREEN

```text
$ go test ./dialect ./internal/sqlsymbol ./internal/handler -run 'TestInterBaseReserved|TestRename|TestLocalRename' -count=1
ok   github.com/sqls-server/sqls/dialect 0.003s
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.004s
ok   github.com/sqls-server/sqls/internal/handler 0.010s
```

## Fix round 3

### Source verification and full-list reconciliation

The previously cited DocWiki PDF was not readable through the external HTTP
fetcher, so I verified the locally installed InterBase documentation instead:

```text
SOURCE=/home/a.simard@multidev.local/.cache/opencode/tmp/interbase/Doc/LangRef.pdf
test -r "$SOURCE" && echo "readable: $SOURCE"
readable: /home/a.simard@multidev.local/.cache/opencode/tmp/interbase/Doc/LangRef.pdf

pdfinfo "$SOURCE" | grep -E '^(Title|Pages|Page size|CreationDate|ModDate)'
Title:           Language Reference Guide
CreationDate:    Wed Apr 22 16:01:35 2020 EDT
Pages:           280
Page size:       595.275 x 841.889 pts (A4)

pdftotext -layout "$SOURCE" /tmp/opencode-interbase-langref.txt
```

The readable text is the InterBase 2020 Language Reference Guide, Appendix
“InterBase Keywords”, printed pages 165–169. Its introductory text says
“These keywords are reserved words in all dialects” and explains the Dialect 3
double-quote exemption. The appendix also includes `OPEN`, `FALSE`, `FETCH`,
`OPTION`, and the previously omitted words. A normalized comparison of the
appendix token block against the implementation reported:

```text
pdftotext appendix token count: 293 unique: 293
non-unique: []
missing from implementation: []
extra in implementation: []
```

The dedicated set now contains exactly those 293 unique vendor words. The
Dialect 1 `TIME`/`TIMESTAMP` exception remains explicit; Dialect 3 accepts
them as reserved words, matching the existing variant semantics.

### RED

The complete-list test was run against the pre-fix implementation (the
committed parent version, temporarily restored and then restored again):

```text
go test ./dialect -run '^TestInterBaseReservedWordsMatchVendorAppendix$' -count=1
--- FAIL: TestInterBaseReservedWordsMatchVendorAppendix (0.00s)
    interbase_test.go:153: InterBase reserved-word set has 163 words, want vendor appendix's 293
FAIL
```

The focused omission tests independently showed the concrete regressions:
`OPEN`, `FALSE`, `FETCH`, and `OPTION` (including lowercase forms) were
accepted by both dialect validation and rename validation.

### GREEN

```text
$ go test ./dialect ./internal/sqlsymbol ./internal/handler -run 'TestInterBaseReserved|TestRename|TestLocalRename' -count=1
ok   github.com/sqls-server/sqls/dialect 0.006s
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.004s
ok   github.com/sqls-server/sqls/internal/handler 0.010s
```

The dialect tests now compare the complete 293-word set, verify Dialect 1/3
`TIME` and `TIMESTAMP` behavior, and retain valid function-like `ABS` and
`DATEADD`. SQL-symbol tests verify representative uppercase/lowercase rename
rejections and allowed function-like replacements.

Additional verification:

```text
$ go test ./dialect ./internal/sqlsymbol ./internal/handler -count=1
ok   github.com/sqls-server/sqls/dialect 0.004s
ok   github.com/sqls-server/sqls/internal/sqlsymbol 0.005s
ok   github.com/sqls-server/sqls/internal/handler 1.118s

$ go test ./... -count=1
all packages passed; handler 1.085s, sqlsymbol 0.010s

$ git diff --check
(no output; exit 0)
```

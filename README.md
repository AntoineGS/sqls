# sqls: SQL Language Server

![test](https://github.com/sqls-server/sqls/workflows/test/badge.svg)

An implementation of the Language Server Protocol for SQL.

## Note

This project is currently under development and there is no stable release. Therefore, destructive interface changes and configuration changes are expected.

## Features

sqls aims to provide advanced intelligence for you to edit sql in your own editor.

### Support RDBMS

- MySQL([Go-MySQL-Driver](https://github.com/go-sql-driver/mysql))
- PostgreSQL([pgx](https://github.com/jackc/pgx))
- SQLite3([go-sqlite3](https://github.com/mattn/go-sqlite3))
- MSSQL([go-mssqldb](https://github.com/microsoft/go-mssqldb))
- H2([pgx](https://github.com/CodinGame/h2go))
- Vertica([vertica-sql-go](https://github.com/vertica/vertica-sql-go))
- InterBase SQL Dialect 1 ([interbase-go](../interbase-go), optional native build)

### Language Server Features

#### Auto Completion

![completion](./imgs/sqls-completion.gif)

- DML(Data Manipulation Language)
    - [x] SELECT
        - [x] Sub Query
    - [x] INSERT
    - [x] UPDATE
    - [x] DELETE
- DDL(Data Definition Language)
    - [ ] CREATE TABLE
    - [ ] ALTER TABLE

#### Join completion
If the tables are connected with a foreign key sqls can complete ```JOIN``` statements

![join_completion](imgs/sqls-fk_joins.gif)

#### CodeAction

![code_actions](https://github.com/sqls-server/sqls.vim/blob/master/imgs/sqls_vim_demo.gif)

- [x] Execute SQL
- [x] Explain SQL
  - InterBase only; other drivers report that the command is unsupported.
- [x] Switch Connection(Selected Database Connection)
- [x] Switch Database

#### Hover

![hover](./imgs/sqls_hover.gif)

#### Signature Help

![signature_help](./imgs/sqls_signature_help.gif)

#### Document Formatting

![document_format](./imgs/sqls_document_format.gif)

## Installation

```shell
go install github.com/sqls-server/sqls@latest
```

### InterBase Build

InterBase connectivity is opt-in. The current integration uses the experimental
local `interbase-go` module through `replace interbase-go => ../interbase-go` in
`go.mod`; keep the driver checkout alongside this repository. It requires
Linux/amd64, Go 1.25.7 or newer, a C compiler, and the official InterBase SDK at
`/opt/interbase/include` with `libgds.so` at `/opt/interbase/lib`. The proprietary
SDK and client library are not distributed with sqls.

Build from this checkout:

```shell
CGO_ENABLED=1 go build -tags interbase -o sqls .
```

Ordinary builds do not link the InterBase client. Selecting an InterBase
connection in such a build reports that the native build is required. The
upstream `go install ...@latest` command does not include this local integration.

#### InterBase editor features

On an InterBase connection sqls reads the database's own catalog and uses it in
five catalog-backed editor surfaces. There are no settings; catalog-backed
behavior falls back to its ordinary behavior when the metadata is unavailable —
on another driver, on a build without the InterBase tag, and in the short window
after connecting before the catalog has been read. In-document procedure
navigation, references and rename are separate document-only features and do not
require catalog metadata. Table/column definitions do require catalog metadata
and reproducible DDL, as described below.

**Explain SQL.** The `Explain SQL` code action shows the query plan InterBase
chose. It **prepares the statement without executing it**: nothing is inserted,
updated or deleted, and a statement that modifies data is shown with a banner
saying so. `SELECT`, `INSERT`, `UPDATE`, `DELETE` and `EXECUTE PROCEDURE` are
supported; DDL and transaction control are refused, because their plan is always
empty and an empty pane reads like a bug. A statement can prepare successfully
and still have no plan — InterBase simply reports none — and that is shown as
text rather than as a blank result. A `SELECT` whose result contains an array
column cannot be explained, because the prepare path sqls uses does not accept
array results.

**Completion.** Procedures are offered after `EXECUTE PROCEDURE`; procedures
that return output are offered wherever a table is, because an InterBase
selectable procedure is legal wherever a relation is; a selectable procedure's
output parameters are offered as its columns. External functions appear beside
the built-in functions, and generators are offered inside a `GEN_ID(` call —
only there, because offering every generator in every expression would bury the
column candidates. Views are labelled `view` rather than `table`. Identifiers
match case-insensitively, so `myproc` finds `MYPROC`.

Procedure **input parameter names** are deliberately not completed: InterBase
DSQL has no named parameters, so a parameter name is never valid text in a
statement. They appear in signature help and hover instead. Triggers are not
completed either — no SQL context in which sqls completes ever names one.

##### Procedure navigation and rename

For the active InterBase dialect, sqls resolves declared procedure variables
and input/output parameters across the containing procedure. Both bare and
colon-prefixed references are supported in their applicable statement contexts.
Rename preserves each occurrence's prefix and distinguishes local assignments
from SQL column targets.

In Neovim's standard LSP mappings, `grr` finds references and `grn` renames.
Use your go-to-definition mapping (commonly `gd`) for local declarations or
catalog-backed table/column definitions. Local references and rename cover the
containing procedure in the current document. Ambiguous rename requests are
rejected rather than applying a partial spelling-based replacement.

Table/column definitions require metadata from the active connection and
reproducible DDL. They use the existing source-snapshot storage and cleanup.
The client needs a rebuilt native `sqls` binary and a restart to advertise the
new references capability.

**Signature help.** Typing an argument list for a known procedure shows its
input parameters and highlights the one under the cursor, both for
`EXECUTE PROCEDURE MYPROC(…)` and for `SELECT * FROM MYPROC(…)`. Output
parameters are never listed as arguments; their count appears in the tooltip
text instead, so a selectable procedure can still be told apart from a purely
executable one. A parameter is marked `NOT NULL` only when the catalog proves
it. Most InterBase procedure parameters carry no declaration nullability at
all, and those are shown with no nullability marking rather than a guess. A
space before the parenthesis — `MYPROC (1, 2)` — is not recognised as a call,
which matches sqls's existing behaviour for built-in functions.

Known limitation: for a call nested inside another call's argument list, such
as `MYPROC(OTHERCALL(1, 2), 3)`, sqls keeps showing `MYPROC`'s signature the
whole time and tracks the active-parameter position from `OTHERCALL`'s own
argument list instead of `MYPROC`'s once the cursor is inside the inner
parentheses — the parser never gives the nested call its own node to hang
correct tracking off. This is misleading in that one case and is not fixable
from the signature-help code alone.

**Hover.** Hovering a table, view, procedure, trigger or generator shows what
the catalog knows about it and then, when InterBase can reproduce it, the real
`CREATE` statement. When it cannot, hover names the reason in one line and
shows the object's verbatim catalog source instead — unless the cache itself is
stale and names an object InterBase no longer has, in which case hover shows
only what the catalog still knows and adds nothing about DDL, rather than a
footnote the user cannot act on. **sqls never invents a `CREATE` statement it
did not get from the database.** DDL that cannot be reproduced is the ordinary
case for procedures: the reference-compatible catalog does not record whether a
parameter was declared nullable, and without that a faithful declaration cannot
be written. Hovering an external function shows its declaration metadata and
never mentions DDL, because InterBase does not reproduce DDL for external
functions at all.

Some catalog values are simply absent — a trigger's event, an external
function's return type, and the type of a `CHAR` or `VARCHAR` function argument,
which InterBase never records. Wherever a value is missing the corresponding
line is left out rather than filled with a placeholder, and the object itself is
still shown.

The DDL lookup happens when you hover, not when you connect: the first hover of
an object makes a database round trip on sqls's own request-handling loop,
bounded at three seconds, and sqls cannot read or answer any other request
until it returns. Hovering the same object again is instant: the rendered
result is kept in memory, one entry per object hovered, for as long as the
connection lasts, and is only cleared when the connection is switched or
reopened.

##### Go-to-definition for database-resident source

Procedures, views, triggers and tables keep their source or DDL in the database,
not in a file on disk. `textDocument/definition` materialises that source as a
**read-only snapshot file**; table and column targets resolve to the
corresponding declaration in that snapshot. It returns an ordinary `file://`
location, so any editor that can open a file can follow the jump — no
client-side content provider and no custom URI scheme.

Snapshots live under the user cache directory, in
`sqls/interbase-sources/<hash>-<pid>/<kind>/<name>.sql`. On Linux that is
`$XDG_CACHE_HOME/sqls/interbase-sources`, or `~/.cache/sqls/interbase-sources`
when `XDG_CACHE_HOME` is unset. There is one directory per connection per server
process; `<hash>` is derived from the connection settings, which are hashed
rather than written so a host name or database path never lands on disk in
clear form. Directories are created with mode `0700` and files with mode
`0600`.

**Editing a snapshot does not change the database.** There is no write-back
path, and there is no cache: every jump performs a fresh catalog round trip,
bounded to 3 seconds, and rewrites the file before returning the location. The
content is therefore never stale, and any local edit is overwritten on the next
jump.

**Snapshots contain your database's business logic, and a crash leaves them on
disk.** They are removed when the server shuts down — including when closing
the database connection fails — but a process that dies without shutting down
(a `SIGKILL`, for example) leaves its directory behind, still mode `0700`. That
directory is removed by the next sqls run that uses this feature, once it is
more than 24 hours old. A directory that is still being written to cannot reach
that age: every snapshot write refreshes its connection directory's
modification time, so a connection genuinely in use never goes stale no matter
how long ago its directory was first created. Because process ids are reused, a
dead directory that happens to share its pid suffix with the process doing the
pruning is skipped that round instead of removed — delayed cleanup of an
already-abandoned directory, not the wrongful deletion of one still in use.
That window is the cost of portable LSP navigation: there is no way to hand an
editor navigable text without a real file. If it is unacceptable in your
environment, delete the `sqls/interbase-sources` directory under the cache
directory above yourself, or do not use go-to-definition on database objects.

When InterBase's catalog cannot reproduce executable DDL — most commonly
because it does not record whether a procedure parameter is nullable — the
snapshot contains the **verbatim catalog source** under a comment naming what
blocked reproduction. A `CREATE` header is never invented. When the object has
disappeared from the catalog since the connection was cached, no file is
written and the editor reports that no definition was found.

Go-to-definition for in-document aliases and subqueries is unchanged and works
for every driver, with or without a catalog.

### Cancelling a running query

`workspace/executeCommand` runs off the request-handling loop, so sqls keeps
answering completion, hover and formatting while a query runs, and an editor
that sends `$/cancelRequest` stops it.

Cancellation is **best effort, not a hard deadline**. A statement that finished
before the cancellation took effect returns its real result, with a note saying
the cancellation arrived too late. For InterBase, a cancelled statement whose
effect could not be established is reported loudly:

> Cancelled, but the outcome is UNCERTAIN.
>
> InterBase could not confirm whether this statement took effect. Do not re-run
> it until you have checked the database state — reconcile by operation id or by
> querying the affected rows.

Do not blindly re-run a cancelled write.

### Query results

Results are rendered from the column metadata the driver reports, so the pane
shows what the database actually returned.

- **`NULL` is not an empty string — but only for InterBase.** For InterBase, a
  SQL `NULL` renders as the literal `NULL` and an empty value renders as an
  empty cell, so the two stay distinguishable. On every other driver, `NULL`
  now renders as an empty cell too, indistinguishable from an empty string
  `''` — previously it rendered as the literal `<nil>`.
- **Exact decimals stay exact.** A scaled `NUMERIC`/`DECIMAL` column is rendered
  from the driver's exact decimal text, never through a float.
- **Large cells are display-capped at 512 characters.** A longer value is cut at
  the cap and marked `…(truncated, N characters)`, where `N` is the real length.
  This is a display limit in sqls, not data loss and not a database limit. A
  binary BLOB is shown as `<BLOB N bytes>` rather than dumped into the table.
- **A failed fetch still shows what it fetched.** If the query dies partway —
  most often because a BLOB exceeds the InterBase driver's 64 MiB
  materialisation limit, which is a hard error rather than a truncation — the
  pane shows the rows that preceded the failure, a `N rows in set (incomplete)`
  footer, and the driver's error text. When the result has a BLOB column, it
  also suggests re-running without that column or selecting a substring of it.
- **Read statements run in a read-only transaction.** For InterBase, `SELECT`
  and friends run inside an explicit read-committed, read-only transaction that
  is opened and released entirely inside sqls.

### Named query parameters

On an InterBase connection a statement may carry **named parameters** —
`:EMPLYID` — and sqls asks for their values before running it. The values are
bound by the driver: the SQL buffer is never edited, and nothing you type is
ever spliced into the statement text. This needs an editor client that speaks
the `getQueryParameters` command described under
[Query parameter protocol](doc/develop.md#query-parameter-protocol); a client
that does not is unaffected, and a statement with no markers takes the same
path it always has.

**Grammar.** A marker is `:` followed by `[A-Za-z_][A-Za-z0-9_$]*`. `::` and
`:=` are not markers, and neither is anything inside a string literal, a quoted
identifier, a `--` line comment or a `/* */` block comment. Names are
case-insensitive: `:EMPLYID`, `:emplyid` and `:EmplyId` are **one** parameter,
prompted once under the spelling that appeared first, and the single value you
enter is bound to every occurrence.

**Types.** You choose a type per parameter, and the wire spellings are:

| Type | Value format |
| --- | --- |
| `text` | any string, including the empty string. Not trimmed |
| `integer` | `int64`, optional sign. Full 64-bit range, no float rounding |
| `number` | finite `float64` decimal, exponent allowed |
| `date` | `2006-01-02` |
| `timestamp` | `2006-01-02 15:04:05[.fraction]`, up to nine fractional digits |
| `boolean` | `true` or `false` |
| `null` | SQL `NULL`; its value must be empty |

Numeric, date, timestamp and boolean values tolerate surrounding whitespace;
`text` does not, and `null` requires an empty value rather than a blank one.
`null` and the text `NULL` are different things: the latter is the
four-character string. A rejected value names the parameter and its type and
never echoes what you typed.

**Exact decimals: use `text` plus a `CAST`.** `number` is a binary `float64`,
so an exact `NUMERIC`/`DECIMAL` target can pick up rounding. Enter the digits as
`text` and let InterBase parse them:

```sql
SELECT * FROM INVOICE WHERE TOTAL = CAST(:TOTAL AS NUMERIC(18,2));
```

The same applies above 2^53: `integer` transports the full `int64` losslessly,
but the *target* type in your SQL decides what survives. Under Dialect 3,
`CAST(:BIG_ID AS NUMERIC(18,0))` keeps `9007199254740993` exactly; under
Dialect 1 that same `NUMERIC(18,0)` is a floating type and does not. Casting
to `VARCHAR` shows the value the driver actually received either way.

**Scope is the selection, and the whole batch is validated first.** Parameters
are discovered in the selected range, or in the whole document when nothing is
selected. Once any statement in that selection carries a marker, the entire
selection must compile before a single statement runs: named markers are
accepted in `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `EXECUTE PROCEDURE` and
`WITH`-prefixed `SELECT` only. A PSQL body or DDL anywhere in a
marker-bearing selection is refused, because there a leading colon is a local
variable reference and not user input. Bare `?` positional markers are refused
in this flow as well; a selection with no named markers keeps the ordinary
unparameterized path, `?` and all.

That last sentence is about the request sqls itself receives. The Neovim
adapter shipped here goes through discovery first, and when discovery returns
an error — which is what a refused selection produces — it reports the message
and stops rather than re-sending the selection unparameterized. So through this
adapter a refused selection does not execute at all; a legacy client that never
asks for discovery still reaches the driver exactly as before.

**Explain never prompts.** `Explain SQL` on a parameterized statement compiles
the markers to positional placeholders and prepares the statement — a plan
needs no values, and nothing is bound, executed or written.

**On some connections, preparing a `CAST` around a parameter fails.** Wrapping a
marker in a `CAST` — `CAST(:NAME AS VARCHAR(30))` — is how you pin its SQL type
when nothing else in the statement implies one. On one of the two InterBase
servers this was tried against, the *prepare* fails for every target type with

> SQLCODE -804: An error was found in the application program input parameters
> for the SQL statement.

while the same server prepares and runs a marker whose type it can infer from
context, such as `WHERE RDB$RELATION_NAME = :REL_NAME`.

**It is not the parameter binding.** `Explain SQL`, which binds nothing at all,
fails identically, and the same statement prepares on the other server through
the same sqls build. Beyond that the cause is undetermined: the driver reports
every failure in its prepare path — which includes its own input-descriptor
call — under one "prepare plan failed" label and does not surface InterBase's
specific reason, and the affected server's engine version was not confirmed.
Treat it as a difference between connections rather than a known engine
restriction. If you hit it, drop the `CAST` and let the compared column supply
the type.

**Values live in the editor, in memory only.** sqls itself keeps nothing: each
submission arrives with its own values and is forgotten when the command
returns. The client is what remembers, so that re-running the same query
prefills what you last entered. In the Neovim adapter shipped with this
integration, that memory is per language-server client, keyed by connection and
by query text, capped at **100 entries with least-recently-used eviction**, and
never written to disk. Switching connections gives you empty prompts for the
other connection and switching back restores the first one's values.
`:SqlsClearParameters` forgets everything remembered for that server, and
stopping the server clears it too. It also releases a prompt sequence the
adapter still considers in progress, which is the way out if a discovery
request never came back and the adapter says a prompt is already running.

"Not written to disk" is about what the adapter stores. The values do travel
in the `executeQuery` request, so if you turn on LSP debug logging — for
example `vim.lsp.set_log_level("debug")` — they are written to the editor's LSP
log like any other request payload.

**Cancelling a prompt executes nothing.** Dismissing any type or value prompt
ends the run without sending an execution request. Once the statement is
running, `$/cancelRequest` applies with the same best-effort semantics as any
other query.

**A stale answer is refused, not guessed at.** Every prompt carries the
connection identity, the connection generation, the selected SQL and a hash of
the whole document. If any of them changed while you were typing — you switched
connection, reconnected, or edited the file **anywhere**, including outside the
selection — the submission is rejected with a message telling you to run the
command again. Nothing is executed. Editing outside the selection is rejected
deliberately: it is still a version of the document you did not review.

The slow one-time catalog load after connecting to a large database remains a
**separate, pre-existing** limitation of the InterBase integration. It is not
caused by this feature and is not fixed by it: the first command after
connecting still waits for that load, parameter prompts included.

### `EXECUTE PROCEDURE` limitation

sqls does not yet look at a procedure's signature, so every `EXECUTE PROCEDURE`
statement is currently run the same way a write statement is run, whether or
not the procedure returns anything.

- A procedure that returns **no output** works today.
- A procedure that **does** return output currently fails: the driver rejects
  running an output-producing procedure this way rather than silently
  discarding its output. If you hit this, the statement is not malformed — sqls
  is just not yet routing `EXECUTE PROCEDURE` by output arity. There is no
  workaround in this version; routing based on the procedure's cached signature
  is planned.

## Editor Plugins

- [sqls.vim](https://github.com/sqls-server/sqls.vim)
- [vscode-sqls](https://github.com/lighttiger2505/vscode-sqls)
- [sqls.nvim](https://github.com/nanotee/sqls.nvim)
- [Emacs LSP mode](https://emacs-lsp.github.io/lsp-mode/page/lsp-sqls/)

## DB Configuration

The connection to the RDBMS is essential to take advantage of the functionality provided by `sqls`.
You need to set the connection to the RDBMS.

### Configuration Methods

There are the following methods for RDBMS connection settings, and they are prioritized in order from the top.
Whichever method you choose, the settings you make will remain the same.

1. Configuration file specified by the `-config` flag
1. `workspace/configuration` set to LSP client
1. Configuration file located in the following location
    - `$XDG_CONFIG_HOME`/sqls/config.yml ("`$HOME`/.config" is used instead of `$XDG_CONFIG_HOME` if it's not set)

### Configuration file sample

```yaml
# Set to true to use lowercase keywords instead of uppercase.
lowercaseKeywords: false
connections:
  - alias: dsn_mysql
    driver: mysql
    dataSourceName: root:root@tcp(127.0.0.1:13306)/world
  - alias: individual_mysql
    driver: mysql
    proto: tcp
    user: root
    passwd: root
    host: 127.0.0.1
    port: 13306
    dbName: world
    params:
      autocommit: "true"
      tls: skip-verify
  - alias: mysql_via_ssh
    driver: mysql
    proto: tcp
    user: admin
    passwd: Q+ACgv12ABx/
    host: 192.168.121.163
    port: 3306
    dbName: world
    sshConfig:
      host: 192.168.121.168
      port: 22
      user: sshuser
      passPhrase: ssspass
      privateKey: /home/sqls-server/.ssh/id_rsa
  - alias: dsn_vertica
    driver: vertica
    dataSourceName: vertica://user:pass@host:5433/dbname
```

### Workspace configuration Sample

- setting example with vim-lsp.

```vim
if executable('sqls')
    augroup LspSqls
        autocmd!
        autocmd User lsp_setup call lsp#register_server({
        \   'name': 'sqls',
        \   'cmd': {server_info->['sqls']},
        \   'whitelist': ['sql'],
        \   'workspace_config': {
        \     'sqls': {
        \       'connections': [
        \         {
        \           'driver': 'mysql',
        \           'dataSourceName': 'root:root@tcp(127.0.0.1:13306)/world',
        \         },
        \         {
        \           'driver': 'postgresql',
        \           'dataSourceName': 'host=127.0.0.1 port=15432 user=postgres password=mysecretpassword1234 dbname=dvdrental sslmode=disable',
        \         },
        \       ],
        \     },
        \   },
        \ })
    augroup END
endif
```

- setting example with coc.nvim.

In `coc-settings.json` opened by `:CocConfig`

```json
{
    "languageserver": {
        "sql": {
            "command": "sqls",
            "args": ["-config", "$HOME/.config/sqls/config.yml"],
            "filetypes": ["sql"],
            "shell": true
        }
    }
}
```

- setting example with [nvim-lspconfig](https://github.com/neovim/nvim-lspconfig/blob/master/doc/server_configurations.md#sqls).

```lua
require'lspconfig'.sqls.setup{
  on_attach = function(client, bufnr)
    require('sqls').on_attach(client, bufnr) -- require sqls.nvim
  end
  settings = {
    sqls = {
      connections = {
        {
          driver = 'mysql',
          dataSourceName = 'root:root@tcp(127.0.0.1:13306)/world',
        },
        {
          driver = 'postgresql',
          dataSourceName = 'host=127.0.0.1 port=15432 user=postgres password=mysecretpassword1234 dbname=dvdrental sslmode=disable',
        },
      },
    },
  },
}
```

- Setting example for Sublime Text 4

  Install the LSP Client by Opening the command palette and run ```Package Control: Install Package```, then select ```LSP```.

  Open ```Preferences > Package Settings > LSP > Settings``` and add the ```"sqls"``` client configuration to the ```"clients"```:
```
{
    "show_diagnostics_count_in_view_status": true,
    "clients": {
        "sqls": {
            "enabled": true,
            "command": ["/path/to/sqls binary"],
            "selector": "source.sql"
        }
    }
}
```

**I'm sorry. Please wait a little longer for other editor settings.**

### Configuration Parameters

The first setting in `connections` is the default connection.

| Key         | Description          |
| ----------- | -------------------- |
| connections | Database connections |

### connections

`dataSourceName` takes precedence over the value set in `proto`, `user`, `passwd`, `host`, `port`, `dbName`, `params`, except for InterBase, where credentials and charset remain separate settings (see below).

| Key            | Description                                 |
| -------------- | ------------------------------------------- |
| alias          | Connection alias name. Optional.            |
| driver         | `mysql`, `postgresql`, `sqlite3`, `mssql`, `h2`, `interbase`. Required. |
| dataSourceName | Data source name.                           |
| proto          | `tcp`, `udp`, `unix`.                       |
| user           | User name                                   |
| passwd         | Password                                    |
| host           | Host                                        |
| port           | Port                                        |
| path           | unix socket path                            |
| dbName         | Database name                               |
| params         | Option params. Optional.                    |
| sshConfig      | ssh config. Optional.                       |

#### sshConfig

| Key        | Description                 |
| ---------- | --------------------------- |
| host       | ssh host. Required.         |
| port       | ssh port. Required.         |
| user       | ssh user. Optional.         |
| privateKey | private key path. Required. |
| passPhrase | passPhrase. Optional.       |

#### DSN (Data Source Name)

See also.

- <https://github.com/go-sql-driver/mysql#dsn-data-source-name>
- <https://pkg.go.dev/github.com/jackc/pgx/v4>
- <https://github.com/mattn/go-sqlite3#connection-string>

#### InterBase

```yaml
connections:
  - alias: interbase_example
    driver: interbase
    dataSourceName: "db.example.test/3050:/srv/interbase/example.ib"
    user: sqls_reader
    passwd: "your-password"
    params:
      charset: UTF8
```

`dataSourceName` is a native InterBase attachment string, not a URL or a
credential-bearing DSN. `user` is required and `passwd` is supplied separately
(an empty password is permitted). Protect configuration files containing
credentials; the example values are placeholders, not environment-variable
references.

Alternatively, supply `host`, optional `port` (default `3050`), and `path`
(or `dbName`) instead of `dataSourceName`. Without a host, the path is used as a
local attachment. `proto` may be omitted or set to `tcp` for a remote attachment.
`params.charset` accepts `UTF8` (the default), `WIN1250`, `WIN1252`, `ISO8859_1`
and `ASCII`. Built-in SSH tunneling is not supported for this driver.

##### interbase

Connection settings only the InterBase driver understands, nested under the
`interbase` key:

| Key            | Description                                                     |
| -------------- | ---------------------------------------------------------------- |
| role           | SQL role activated for the attachment. Optional, 255 bytes max.  |
| connectTimeout | Go duration bounding the native handshake, e.g. `10s`. Optional. |
| tls            | Native client TLS attachment options. Optional.                  |

| tls key              | Description                                                |
| -------------------- | ----------------------------------------------------------- |
| enabled              | Required before any other `tls` key is accepted.           |
| serverPublicFile     | Server public certificate file.                            |
| serverPublicPath     | Directory searched for server public certificates.         |
| clientCertFile       | Client certificate file.                                   |
| clientPassPhrase     | Client certificate passphrase, stored in the config file.  |
| clientPassPhraseFile | File holding the client certificate passphrase.            |

```yaml
connections:
  - alias: centrale
    driver: interbase
    host: db.example.test
    port: 3050
    path: /srv/interbase/centrale.ib
    user: sqls_reader
    passwd: "your-password"
    params:
      charset: WIN1252
    interbase:
      role: SQLS_READONLY
      connectTimeout: 10s
      tls:
        enabled: true
        serverPublicFile: /etc/interbase/server.pem
        clientPassPhraseFile: /etc/interbase/client.pass
```

`connectTimeout` rounds up to whole seconds: `500ms` becomes `1s`. Setting it
to `0`, or omitting it, leaves the native client's own default handshake
timeout unchanged rather than setting a timeout of zero.

`interbase.tls` requires `host` and cannot be combined with `dataSourceName`:
the driver composes the TLS attachment itself from the host and the database
path, and rejects TLS options when no host is set. `dataSourceName` keeps
working as a raw attachment string for every connection that does not use TLS.

**Enabling TLS does not establish server identity.** The InterBase native client
tested with this driver (`LI-V15.1.0.42`) does not verify the server hostname:
with a trusted CA it still accepted an intentionally wrong DNS name, including
through the vendor `isql`. Enabling `interbase.tls` therefore encrypts the
connection but is not proof of server identity, and sqls does not add a
verification step of its own, because a separate Go-side TLS probe would not
authenticate the native attachment. Treat the network as untrusted accordingly.
Prefer `clientPassPhraseFile` over `clientPassPhrase` so the passphrase is not
stored in `config.yml`.

Both SQL Dialect 1 and SQL Dialect 3 are supported. The optional `dialect` key
accepts `0` (the default, auto-detect from the database), `1` or `3`:

```yaml
connections:
  - alias: interbase_example
    driver: interbase
    dataSourceName: "db.example.test/3050:/srv/interbase/example.ib"
    user: sqls_reader
    passwd: "your-password"
    dialect: 0            # 0 auto-detect (default), 1, or 3
    params:
      charset: UTF8
```

Auto-detection asks the server which dialect the database uses and costs one
extra attachment only for a Dialect 1 database; a Dialect 3 database is detected
on the first attachment. Pinning `dialect:` is only needed to override
auto-detection. A pinned dialect that disagrees with the database is a supported
InterBase configuration — it is how Dialect 3 tooling reads a Dialect 1 database
during a migration — so sqls connects and warns rather than refusing. The
warning appears as an editor notification when sqls first connects or reconnects
after a workspace configuration change; switching the active connection or
database from a command logs it instead. If the server cannot answer, sqls
keeps a pinned dialect, or falls back to Dialect 3 for auto-detect, and warns
either way.

Under Dialect 1, double quotes delimit strings and `DATE` carries a time
component. Under Dialect 3, double quotes delimit identifiers, so `"My Column"`
is a column name, and `TIMESTAMP` is distinct from `DATE`. Completion and
hover render that distinction: a `TIMESTAMP` column shows as `DATE` under
Dialect 1 and as `TIMESTAMP` under Dialect 3. In both dialects, unquoted
identifiers may contain `$`, positional parameters use `?`, and doubled
quotes are preserved verbatim by the formatter, so formatting never rewrites
`'c''d'`. Parsing, completion and formatting use the resolved dialect's
rules when the selected connection is InterBase. The execute command also
prompts for the values of any `:NAME` markers in the selection and binds them
as driver parameters — see
[Named query parameters](#named-query-parameters).

Completion and hover use tables, views, columns, and primary and foreign
keys. The cache also holds procedures with their parameters, triggers,
generators, domains, indexes, and external-function declarations, refreshed
by the background worker after the first connection rather than on demand.
sqls can also reproduce object DDL from the catalog; it is unavailable for
external functions, database files, shadows, tables with computed columns,
and procedures whose parameter nullability the catalog does not record, and
no editor-facing feature surfaces it yet.

An InterBase connection holds a single attachment: `showDatabases` lists that
attachment string, and `switchDatabase` accepts only that same name. Configure
a separate connection entry to open another database. InterBase has no schema
namespace, so `showSchemas` reports one synthetic empty schema.

The native driver is experimental. Context cancellation cannot interrupt an
in-flight native call. Use a least-privilege database account, and read the TLS
statement above before relying on `interbase.tls` for transport security.
Executing DML/DDL uses the driver's implicit commit behavior; SQL
transaction-control statements are not supported.

Run the offline suite and the native-enabled suite with:

```shell
go test ./...
CGO_ENABLED=1 go test -tags interbase ./...
```

The native suite includes a read-only live test that skips unless
`INTERBASE_DATABASE`, `INTERBASE_USER`, and `INTERBASE_PASSWORD` are explicitly
set in the environment (`INTERBASE_PASSWORD` may be empty). It does not retrieve
credentials from other tools or create database fixtures. To run it separately
with an outer timeout for native calls:

```shell
timeout 60s go test -tags interbase ./internal/database -run '^TestInterBaseLive' -count=1 -v -timeout=50s
```

## Contributors

This project exists thanks to all the people who contribute.
<a href="https://github.com/sqls-server/sqls/graphs/contributors">
    <img src="https://contrib.rocks/image?repo=sqls-server/sqls" />
</a>

## Inspired

I created sqls inspired by the following OSS.

- [dbcli Tools](https://github.com/dbcli)
    - [mycli](https://www.mycli.net/)
    - [pgcli](https://www.pgcli.com/)
    - [litecli](https://litecli.com/)
- non-validating SQL parser
    - [sqlparse](https://github.com/andialbrecht/sqlparse)

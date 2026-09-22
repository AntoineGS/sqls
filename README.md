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
- [ ] Explain SQL
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
rules when the selected connection is InterBase. Parameter binding is a
driver capability; the sqls execute command does not prompt for parameter
values.

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

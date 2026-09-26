# Embedding SQLs language services in-process

SQLs exposes a transport-neutral language core through the public Go packages
`language`, `metadata`, `catalog`, and `dialect`. An application can import
those packages directly; it does not need SQLs's LSP server, Wails, or an
`internal` package. The language core does not open a database. The host owns
database attachment, authorization, cancellation, and the lifetime of each
metadata consumer.

## External-module compile/run example

The following was compiled as a separate Go module (not a package inside this
repository) against SQLs HEAD `40f6dc0`. The example uses an offline provider to
prove the language-only path has no native-driver attachment. A production
application replaces that provider factory with its own engine adapter; it
should continue to pass the acquired lease and the lease's current snapshot
explicitly on each request.

Create `/tmp/opencode/b4-public-consumer/go.mod`:

```go
module example.com/b4-public-consumer

go 1.25.7

require github.com/sqls-server/sqls v0.0.0
```

For local feature-branch validation, make a workspace containing the external
module, SQLs, the compatible driver checkout, and the host module. In this
environment, a short symlink avoids special characters in the absolute
checkout path when writing the workspace replacement:

```sh
mkdir -p /tmp/opencode/b4-public-consumer
ln -sfn "$SQLS" /tmp/opencode/sqls
cat >/tmp/opencode/b4-public-consumer/go.work <<EOF
go 1.27.1

use (
  /tmp/opencode/b4-public-consumer
  $SQLS
  $DRIVER
  $APP
)

replace github.com/sqls-server/sqls v0.0.0 => /tmp/opencode/sqls
EOF
```

`SQLS`, `DRIVER`, and `APP` are absolute checkout paths. This workspace is
temporary and untracked; do not add checkout-specific paths to SQLS or
MDExplorer's tracked `go.mod`/`go.work`. For a published SQLs version, use its
published version in `require` and omit the local workspace replacement.

The following complete `main.go` constructs a URL-keyed registry lease,
injects its snapshot and Detail reader into an explicit request, and calls
completion and diagnostics without opening a database:

```go
package main

import (
    "context"
    "fmt"

    "github.com/sqls-server/sqls/catalog"
    "github.com/sqls-server/sqls/dialect"
    "github.com/sqls-server/sqls/language"
    "github.com/sqls-server/sqls/metadata"
)

type offlineProvider struct{}

func (offlineProvider) Plan() catalog.MetadataPlan {
    return catalog.MetadataPlan{Parallelism: 1}
}
func (offlineProvider) DetailPlan(metadata.Request) (metadata.DetailPlan, error) {
    return metadata.DetailPlan{}, metadata.ErrUnsupported
}
func (offlineProvider) Close() error { return nil }

func main() {
    ctx := context.Background()
    registry := metadata.NewRegistry(ctx, metadata.Options{})
    defer registry.Close(context.Background())

    lease, err := registry.Acquire(ctx,
        metadata.Key{Engine: "interbase", URL: "fixture://offline/example"},
        metadata.Scope{Principal: "example-user", TextPolicy: "CP1252"},
        func(context.Context) (metadata.Provider, error) {
            return offlineProvider{}, nil
        },
    )
    if err != nil {
        panic(err)
    }
    defer lease.Release()

    document := language.Document{
        URI: "memory:example.sql", Text: "SEL", Version: 1, Binding: 1,
    }
    request := language.Request{
        Document: document,
        Context: language.Context{
            Key:             lease.Key(),
            Incarnation:     lease.Incarnation(),
            Snapshot:        lease.Snapshot(),
            Variant: dialect.DriverVariant{
                Driver: dialect.DatabaseDriverInterBase,
                Variant: dialect.SQLVariantInterBase3,
            },
            DialectResolved: true,
            Details:         lease,
        },
    }

    service := language.NewService()
    completion, err := service.Complete(ctx, request,
        language.Position{Line: 0, UTF16Column: 3})
    if err != nil {
        panic(err)
    }
    diagnostics, err := service.Diagnostics(ctx, request)
    if err != nil {
        panic(err)
    }
    fmt.Printf("completion candidates: %d; diagnostics: %d\n",
        len(completion.Value.Items), len(diagnostics.Value))

    // Binding tracks the host's document-to-database association, separately
    // from text Version. A rebind rejects an otherwise identical old result.
    document.Binding++
    fmt.Printf("current after binding change: %t\n",
        language.Current(completion.Stamp, document, request.Context))
}
```

Run from the external module directory:

```sh
GOWORK=/tmp/opencode/b4-public-consumer/go.work go test ./... -count=1
GOWORK=/tmp/opencode/b4-public-consumer/go.work go run .
```

Expected output includes one language completion candidate, zero diagnostics,
and `current after binding change: false`. `Current` compares document URI,
version and binding, the store incarnation/key, and captured metadata
generation/revision. Hosts should also capture text/dialect revision inputs
not represented by those public stamp fields where applicable, and fence
publication against their live state.

## Host ownership requirements

- Use one `metadata.Registry` owned by the host runtime. Acquire leases with
  `metadata.Key{Engine, URL}` and a credential-free visibility `Scope`; the
  URL is the normalized database target, not a profile/tab/connection alias.
  Separate connections/tabs to the same target and scope acquire the same
  store. Release each lease once it is no longer used; close the host registry
  on shutdown.
- For every language operation, capture that consumer's document and binding,
  the lease key/incarnation, and one immutable `Lease.Snapshot()`. Put those
  values and the lease as `Context.Details` into that request. Do not use a
  package-global “current connection”; two editors bound to different targets
  must remain independently routable even when focus changes.
- Capture the key, incarnation, snapshot, scope-specific detail reader and
  editor binding from the **same authorized lease** under the host's binding
  synchronization boundary. A same-URL rebind to another role/visibility scope
  is a new binding even when both stores happen to report the same metadata
  generation. Never combine a snapshot captured before rebind with details or
  provenance read from the replacement lease; fence diagnostics and source
  materialization against the binding again immediately before publication.
- The sample provider has an empty metadata plan and unsupported detail by
  design. A production engine provider performs attachment and catalog reads
  only through host-selected adapters and the registry lifecycle. Merely
  constructing `language.Service`/`Request` never attaches to a database.
- MDExplorer's internal InterBase attachments use charset `NONE` to preserve
  its existing CP1252 SQL/display policy; that is host attachment policy, not
  a language-core setting. Do not add a login charset selector. The reusable
  driver's default remains UTF-8.
- Do not read SQLs repository source files at runtime or reach into `internal`
  packages. The external consumer imports only SQLs's public package APIs;
  the language core itself may of course have implementation dependencies
  within SQLs. The example's imports and build do not include a driver package
  or database credentials.

The checked-in `language/public_api_test.go` separately pins API construction
inside SQLs. The external module above verifies the distinct external-module
boundary, including actual registry/lease injection and execution of both
language calls.

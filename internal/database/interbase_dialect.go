package database

import "fmt"

// interBaseDialectDecision is the outcome of resolving the SQL dialect for a
// connection: the dialect to use, whether the caller must re-attach at it, and
// any non-fatal diagnostics for the user.
type interBaseDialectDecision struct {
	// Resolved is 1 or 3.
	Resolved int
	// Reattach is true only when the first attachment used the driver default
	// and the database turned out to be Dialect 1, so it must be replaced.
	Reattach bool
	// Warnings are single-line, credential-free messages for the user.
	Warnings []string
}

// resolveInterBaseDialect decides which SQL dialect a connection uses.
//
// requested is DBConfig.Dialect: 0 auto-detects, 1 and 3 pin a dialect.
// reported is interbase.DatabaseDiagnostics.SQLDialect, the value the server
// answers for this attachment; it is ignored when diagErr is non-nil.
//
// Auto-detect costs exactly one extra attach, and only for a Dialect 1
// database: the first attachment already used the driver default of 3, so a
// reported 3 needs no second attach.
//
// A pinned dialect that disagrees with the database is a warning, not a
// failure. Attaching a client at a dialect different from the database's own is
// a supported InterBase configuration — it is the normal way to read a Dialect
// 1 database from Dialect 3 tooling during a migration — the server enforces
// its own rules regardless, and refusing to attach would take away a working
// setup the user asked for explicitly.
//
// A diagnostics failure is never fatal either: metadata introspection must not
// block editing.
func resolveInterBaseDialect(alias string, requested int, reported int64, diagErr error) interBaseDialectDecision {
	switch {
	case diagErr != nil:
		resolved := requested
		if resolved == 0 {
			resolved = 3
		}
		return interBaseDialectDecision{
			Resolved: resolved,
			Warnings: []string{fmt.Sprintf(
				"interbase: connection %q could not read database diagnostics (%v); using SQL dialect %d.",
				alias, diagErr, resolved,
			)},
		}

	case requested == 0 && reported == 1:
		return interBaseDialectDecision{Resolved: 1, Reattach: true}

	case requested == 0 && reported == 3:
		return interBaseDialectDecision{Resolved: 3}

	case requested == 0:
		return interBaseDialectDecision{
			Resolved: 3,
			Warnings: []string{fmt.Sprintf(
				"interbase: connection %q reports an unexpected SQL dialect %d; using SQL dialect 3.",
				alias, reported,
			)},
		}

	case int64(requested) != reported:
		return interBaseDialectDecision{
			Resolved: requested,
			Warnings: []string{fmt.Sprintf(
				"interbase: connection %q is configured for SQL dialect %d but the database reports SQL dialect %d; "+
					"sqls will lex and render types as dialect %d. Remove `dialect` or set `dialect: 0` to follow the database.",
				alias, requested, reported, requested,
			)},
		}

	default:
		return interBaseDialectDecision{Resolved: requested}
	}
}

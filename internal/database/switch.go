package database

import "context"

// DatabaseSwitchRepository is implemented by repositories whose connection is
// bound to a fixed set of databases. The switchDatabase command consults it
// before reconnecting; a repository that does not implement it accepts every
// name, which is the existing behavior for every driver that enumerates
// databases. DBRepository is unchanged: callers type-assert, and absence is the
// normal case.
type DatabaseSwitchRepository interface {
	// ValidateDatabaseSwitch returns nil when this connection can serve name and
	// an explanatory error when it cannot.
	ValidateDatabaseSwitch(ctx context.Context, name string) error
}

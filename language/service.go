package language

// Current reports whether stamp is still coherent with document and
// context: URI, edit Version, database Binding, store Incarnation, catalog
// Key and — when a snapshot is known — Generation and Revision must all
// match exactly. A nil context.Snapshot compares as Generation and
// Revision zero, matching the zero value a Stamp gets before any metadata
// snapshot was ever observed.
//
// The Service type that produces Stamps, and its Complete/Hover/Definition
// and other methods, are defined in completion.go and this package's other
// per-operation files. Current is usable standalone by a host that only
// needs to fence its own previously cached Results.
func Current(stamp Stamp, document Document, context Context) bool {
	generation, revision := uint64(0), uint64(0)
	if context.Snapshot != nil {
		generation = context.Snapshot.Generation
		revision = context.Snapshot.Revision
	}
	return stamp.URI == document.URI &&
		stamp.Version == document.Version &&
		stamp.Binding == document.Binding &&
		stamp.Incarnation == context.Incarnation &&
		stamp.Generation == generation &&
		stamp.Revision == revision &&
		stamp.Key == context.Key
}

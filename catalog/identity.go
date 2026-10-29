package catalog

import "strconv"

// ObjectID is an exact, case-preserving catalog object identity. It is a
// parallel, more precise identity to Cache's existing upper-casing lookup
// maps; it does not replace them.
type ObjectID struct {
	Catalog, Schema, Owner string
	Kind                   ObjectKind
	Name                   string
	// Parent qualifies relation-scoped objects such as schema-bearing indexes.
	// It is omitted from keys when empty to preserve existing identities.
	Parent string
}

// Key returns an unambiguous identity string. Each component is
// length-prefixed before its bytes so a boundary shift between adjacent
// components (for example Schema="A",Name="BC" versus Schema="AB",Name="C")
// can never produce the same key.
func (id ObjectID) Key() string {
	var out []byte
	components := []string{id.Catalog, id.Schema, id.Owner, string(id.Kind), id.Name}
	if id.Parent != "" {
		components = append(components, id.Parent)
	}
	for _, component := range components {
		out = strconv.AppendInt(out, int64(len(component)), 10)
		out = append(out, ':')
		out = append(out, component...)
	}
	return string(out)
}

// IndexObjectID returns an index identity. Schema-bearing indexes are
// qualified by their parent relation because engines such as MySQL allow the
// same index name on different tables. Schema-less providers retain the
// historical (kind, name) identity used by InterBase and Firebird.
func IndexObjectID(schema, relation, name string) ObjectID {
	id := ObjectID{Schema: schema, Kind: ObjectKindIndex, Name: name}
	if schema != "" {
		id.Parent = relation
	}
	return id
}

package catalog

import "strconv"

// ObjectID is an exact, case-preserving catalog object identity. It is a
// parallel, more precise identity to Cache's existing upper-casing lookup
// maps; it does not replace them.
type ObjectID struct {
	Catalog, Schema, Owner string
	Kind                   ObjectKind
	Name                   string
}

// Key returns an unambiguous identity string. Each component is
// length-prefixed before its bytes so a boundary shift between adjacent
// components (for example Schema="A",Name="BC" versus Schema="AB",Name="C")
// can never produce the same key.
func (id ObjectID) Key() string {
	var out []byte
	for _, component := range []string{id.Catalog, id.Schema, id.Owner, string(id.Kind), id.Name} {
		out = strconv.AppendInt(out, int64(len(component)), 10)
		out = append(out, ':')
		out = append(out, component...)
	}
	return string(out)
}

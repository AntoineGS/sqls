package catalog

import "errors"

// ErrAmbiguous reports that an unquoted (dialect-folded) name matched more
// than one exact object identity of the same Kind, and no exact-identity
// match resolved it unconditionally. Callers use errors.Is(err, ErrAmbiguous)
// rather than parsing an error message to detect this case.
var ErrAmbiguous = errors.New("catalog: object name is ambiguous")

// ObjectIndex resolves catalog objects both by their exact ObjectID and by an
// unquoted (dialect-folded) name. It is a lookup structure only: it never
// stores descriptor data, and inserting the same ObjectID twice is a no-op.
//
// Exact identity is preserved unconditionally: Lookup always returns exactly
// the object that was inserted under that ObjectID, regardless of how many
// other objects fold to the same unquoted name. LookupUnquoted deliberately
// does not pick a winner when more than one object of the same Kind folds to
// the same name in different namespaces (Catalog/Schema/Owner/Parent): it
// returns every match, so a caller can detect the ambiguity instead of
// silently resolving to one arbitrary object.
type ObjectIndex struct {
	fold   func(string) string
	exact  map[string]ObjectID
	byFold map[foldKey][]ObjectID
}

type foldKey struct {
	kind   ObjectKind
	folded string
}

// NewObjectIndex returns an empty index. fold normalizes an unquoted name for
// lookup purposes; a nil fold defaults to InterBase's convention of folding
// unquoted identifiers to upper case. A different engine can supply its own
// folding rule (for example, lower-casing) without changing this type.
func NewObjectIndex(fold func(string) string) *ObjectIndex {
	if fold == nil {
		fold = defaultFold
	}
	return &ObjectIndex{
		fold:   fold,
		exact:  make(map[string]ObjectID),
		byFold: make(map[foldKey][]ObjectID),
	}
}

func defaultFold(name string) string {
	out := make([]byte, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// Insert adds id to the index, keyed by its exact identity and by its folded
// (Kind, Name) for unquoted lookup. Inserting the same exact identity again
// is a no-op.
func (idx *ObjectIndex) Insert(id ObjectID) {
	if idx == nil {
		return
	}
	key := id.Key()
	if _, exists := idx.exact[key]; exists {
		return
	}
	idx.exact[key] = id
	fk := foldKey{kind: id.Kind, folded: idx.fold(id.Name)}
	idx.byFold[fk] = append(idx.byFold[fk], id)
}

// Lookup returns the object previously inserted under id's exact identity.
func (idx *ObjectIndex) Lookup(id ObjectID) (ObjectID, bool) {
	if idx == nil {
		return ObjectID{}, false
	}
	got, ok := idx.exact[id.Key()]
	return got, ok
}

// LookupUnquoted returns every object of kind whose Name folds to the same
// value as name. Zero results means no such object; exactly one result means
// the lookup is unambiguous; more than one result means name is ambiguous
// across namespaces and the caller must disambiguate explicitly (for example
// by schema) rather than choosing one arbitrarily.
func (idx *ObjectIndex) LookupUnquoted(kind ObjectKind, name string) []ObjectID {
	if idx == nil {
		return nil
	}
	matches := idx.byFold[foldKey{kind: kind, folded: idx.fold(name)}]
	if len(matches) == 0 {
		return nil
	}
	return append([]ObjectID(nil), matches...)
}

package database

import (
	"context"
	"sort"
	"strings"

	"github.com/sqls-server/sqls/catalog"
	"interbase-go/schema"
)

type watchedSelection struct {
	Owners           map[catalog.MetadataKind][]catalog.ObjectID
	Deleted          []catalog.ObjectID
	DependencyOwners []catalog.DependencyRef
	Affected         int
	FullReason       string
	// Expected contains only eligible candidate headers. Empty scoped results for
	// these identities are incomplete, never evidence of deletion.
	expected         map[catalog.MetadataKind]map[catalog.ObjectID]bool
	expectedChildren map[catalog.MetadataKind]map[catalog.ObjectID]int
	dependencyEdges  []catalog.DependencyEdge
	checkNames       map[string][]string
}

type watchedRouting struct {
	headers          map[string]schema.FingerprintRecord
	forward, reverse map[schema.CatalogObject][]schema.CatalogLink
	views            map[string]bool
}

// One shared transient budget covers both routing indexes and selection. The
// conservative charges include map buckets/growth, slice spare capacity and
// compact-record backing slices/strings (not merely their pointer sizes).
// Exhaustion selects the existing safe mapping fallback, never a partial delta.
const watchedMappingBudget = 576 << 20

type watchedMappingWork struct {
	ctx       context.Context
	err       error
	steps     uint64
	bytes     int64
	exhausted bool
}

func (w *watchedMappingWork) step() bool {
	if w.err != nil || w.exhausted {
		return false
	}
	if w.steps%1024 == 0 {
		w.err = w.ctx.Err()
	}
	w.steps++
	return w.err == nil
}

func (w *watchedMappingWork) charge(bytes int64) bool {
	if bytes > watchedMappingBudget-w.bytes {
		w.exhausted = true
		return false
	}
	w.bytes += bytes
	return true
}

func watchedObjectBytes(o schema.CatalogObject) int64 {
	return int64(len(o.Kind) + len(o.Name) + len(o.Parent))
}

func watchedLinkBytes(l schema.CatalogLink) int64 {
	return watchedObjectBytes(l.From) + watchedObjectBytes(l.To) + int64(len(l.Field))
}

func watchedObject(o schema.CatalogObject) schema.CatalogObject {
	o.NativeType.Valid = false
	o.NativeType.Int64 = 0
	if o.Kind == "view" {
		o.Kind = "relation"
	}
	return o
}
func watchedHeaderKey(o schema.CatalogObject) string {
	o = watchedObject(o)
	return o.Kind + "\x00" + o.Name
}
func watchedHeaderFamily(f string) bool {
	switch f {
	case "RDB$RELATIONS", "RDB$FIELDS", "RDB$PROCEDURES", "RDB$TRIGGERS", "RDB$INDICES", "RDB$GENERATORS", "RDB$EXCEPTIONS", "RDB$FUNCTIONS":
		return true
	}
	return false
}

// A covered empty descriptor currently means physical deletion to MergeObjects.
// Persistent headers crossing descriptor categories therefore need the accepted
// full path. NULL and zero have identical COALESCE eligibility; generated domain
// names remain excluded regardless of their raw flag.
func watchedHeaderCategory(row schema.FingerprintRecord) string {
	if len(row.Owners) == 0 {
		return ""
	}
	owner := row.Owners[0]
	// The existing Exceptions accessor has no SYSTEM_FLAG predicate.
	if owner.Kind == "exception" {
		return owner.Kind
	}
	if owner.Kind == "domain" && strings.HasPrefix(owner.Name, "RDB$") {
		return "excluded"
	}
	if row.SystemFlag.Valid && row.SystemFlag.Int64 != 0 {
		if row.Family == "RDB$RELATIONS" {
			return "system relation"
		}
		return "excluded"
	}
	return owner.Kind
}
func watchedRoutingFor(f *schema.CatalogFingerprint, work *watchedMappingWork) watchedRouting {
	r := watchedRouting{headers: map[string]schema.FingerprintRecord{}, forward: map[schema.CatalogObject][]schema.CatalogLink{}, reverse: map[schema.CatalogObject][]schema.CatalogLink{}, views: map[string]bool{}}
	f.VisitRecords(func(row schema.FingerprintRecord) bool {
		if !work.step() {
			return false
		}
		if watchedHeaderFamily(row.Family) && len(row.Owners) > 0 {
			bytes := int64(1024 + 2*(len(row.Family)+len(row.Key)) + 256*len(row.Owners) + 512*len(row.Links))
			for _, o := range row.Owners {
				if !work.step() {
					return false
				}
				bytes += 2 * watchedObjectBytes(o)
			}
			for _, l := range row.Links {
				if !work.step() {
					return false
				}
				bytes += 2 * watchedLinkBytes(l)
			}
			if !work.charge(bytes) {
				return false
			}
			r.headers[watchedHeaderKey(row.Owners[0])] = row
		}
		if row.Family != "RDB$DEPENDENCIES" {
			for _, o := range row.Owners {
				if !work.step() {
					return false
				}
				if o.Kind == "view" {
					if !work.charge(int64(256 + 2*len(o.Name))) {
						return false
					}
					r.views[o.Name] = true
				}
			}
		}
		if row.Family != "RDB$DEPENDENCIES" {
			for _, link := range row.Links {
				if !work.step() || !work.charge(1024+4*watchedLinkBytes(link)) {
					return false
				}
				a, b := watchedObject(link.From), watchedObject(link.To)
				r.forward[a] = append(r.forward[a], link)
				r.reverse[b] = append(r.reverse[b], link)
			}
		}
		return true
	})
	return r
}

func mapWatchedChanges(ctx context.Context, base *catalog.Cache, previous, current *schema.CatalogFingerprint) (selection watchedSelection, err error) {
	work := &watchedMappingWork{ctx: ctx}
	defer func() {
		if work.err == nil {
			work.err = ctx.Err()
		}
		if work.err != nil {
			selection, err = watchedSelection{}, work.err
		} else if work.exhausted {
			selection, err = watchedSelection{FullReason: "mapping"}, nil
		}
	}()
	full := func(reason string) (watchedSelection, error) { return watchedSelection{FullReason: reason}, nil }
	if previous == nil || current == nil || previous.Version != current.Version || base == nil {
		return full("mapping")
	}
	old := watchedRoutingFor(previous, work)
	if work.err != nil || work.exhausted {
		return full("mapping")
	}
	new := watchedRoutingFor(current, work)
	if work.err != nil || work.exhausted {
		return full("mapping")
	}
	sel := watchedSelection{Owners: map[catalog.MetadataKind][]catalog.ObjectID{}, expected: map[catalog.MetadataKind]map[catalog.ObjectID]bool{}, checkNames: map[string][]string{}}
	owners := map[catalog.MetadataKind]map[catalog.ObjectID]bool{}
	descriptors := map[catalog.ObjectID]bool{}
	deleted := map[catalog.ObjectID]bool{}
	deps := map[catalog.DependencyRef]bool{}
	relations := map[string]bool{}
	reason := ""
	cover := func(kind catalog.MetadataKind, id catalog.ObjectID, expected bool) {
		if !work.step() || !work.charge(int64(1024+4*(len(kind)+len(id.Kind)+len(id.Name)))) {
			return
		}
		if owners[kind] == nil {
			owners[kind] = map[catalog.ObjectID]bool{}
		}
		owners[kind][id] = true
		if expected {
			if sel.expected[kind] == nil {
				sel.expected[kind] = map[catalog.ObjectID]bool{}
			}
			sel.expected[kind][id] = true
		}
	}
	eligible := func(o schema.CatalogObject, r watchedRouting) bool {
		if o.Kind == "exception" {
			_, exists := r.headers[watchedHeaderKey(o)]
			_, was := old.headers[watchedHeaderKey(o)]
			_, now := new.headers[watchedHeaderKey(o)]
			return exists || !was && !now
		}
		h, ok := r.headers[watchedHeaderKey(o)]
		if o.Kind == "domain" && strings.HasPrefix(o.Name, "RDB$") {
			return false
		}
		if !ok {
			_, was := old.headers[watchedHeaderKey(o)]
			_, now := new.headers[watchedHeaderKey(o)]
			return !was && !now
		}
		return !h.SystemFlag.Valid || h.SystemFlag.Int64 == 0
	}
	isView := func(o schema.CatalogObject, r watchedRouting) bool {
		return o.Kind == "view" || r.views[o.Name] && eligible(o, r)
	}
	var ref func(schema.CatalogObject, watchedRouting) (catalog.DependencyRef, bool)
	ref = func(o schema.CatalogObject, r watchedRouting) (catalog.DependencyRef, bool) {
		if !work.step() {
			return catalog.DependencyRef{}, false
		}
		if o.NativeType.Valid {
			switch schema.DependencyObjectType(o.NativeType.Int64) {
			case schema.DependencyRelation:
				o.Kind = "relation"
			case schema.DependencyView:
				o.Kind = "view"
			case schema.DependencyTrigger:
				o.Kind = "trigger"
			case schema.DependencyComputedField:
				var result catalog.DependencyRef
				for _, link := range r.reverse[schema.CatalogObject{Kind: "domain", Name: o.Name}] {
					if !work.step() {
						return catalog.DependencyRef{}, false
					}
					if watchedObject(link.From).Kind == "relation" {
						v := catalog.DependencyRef{Kind: catalog.ObjectKindColumn, Name: link.Field, Parent: link.From.Name}
						if result.Name != "" && result != v {
							return result, false
						}
						result = v
					}
				}
				if result.Name != "" {
					// The full graph maps only non-NULL COMPUTED_SOURCE domains.
					// That scalar is hashed, not retained by the compact DTO. Reuse
					// its existing cache fact only with the same exact domain digest;
					// otherwise this group's classification is unprovable.
					key := watchedHeaderKey(schema.CatalogObject{Kind: "domain", Name: o.Name})
					baseline, was := old.headers[key]
					header, exists := r.headers[key]
					if !was || !exists || baseline.Digest != header.Digest || !base.MetadataReady(catalog.MetadataColumnsCurrent) {
						return result, false
					}
					for _, column := range base.ColumnsByID[catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: result.Parent}.Key()] {
						if !work.step() {
							return catalog.DependencyRef{}, false
						}
						if column.Name == result.Name && column.Facts != nil && column.Facts.DomainName.Valid && column.Facts.DomainName.String == o.Name && column.Facts.ComputedSource.Valid {
							return result, true
						}
					}
					return result, false
				}
				return catalog.DependencyRef{}, false
			case schema.DependencyProcedure:
				o.Kind = "procedure"
			case schema.DependencyException:
				o.Kind = "exception"
			case schema.DependencyGenerator:
				o.Kind = "generator"
			case schema.DependencyFunction:
				o.Kind = "function"
			default:
				return catalog.DependencyRef{}, false
			}
		}
		if o.Kind == "trigger" {
			for _, link := range r.reverse[watchedObject(o)] {
				if !work.step() {
					return catalog.DependencyRef{}, false
				}
				if link.Field == "CHECK" {
					for _, parent := range r.forward[watchedObject(link.From)] {
						if !work.step() {
							return catalog.DependencyRef{}, false
						}
						if watchedObject(parent.To).Kind == "relation" && strings.TrimRight(parent.Field, " ") == "CHECK" {
							return catalog.DependencyRef{Kind: catalog.ObjectKindConstraint, Name: link.From.Name, Parent: parent.To.Name}, true
						}
					}
				}
			}
		}
		kind := catalog.ObjectKind(o.Kind)
		if o.Kind == "relation" {
			kind = catalog.ObjectKindTable
			if isView(o, r) {
				kind = catalog.ObjectKindView
			}
		}
		return catalog.DependencyRef{Kind: kind, Name: o.Name, Parent: o.Parent}, o.Name != ""
	}
	addDep := func(o schema.CatalogObject, r watchedRouting) {
		if !work.step() {
			return
		}
		v, ok := ref(o, r)
		if !ok {
			reason = "mapping"
			return
		}
		if !deps[v] && !work.charge(int64(512+2*(len(v.Kind)+len(v.Name)+len(v.Parent)))) {
			return
		}
		deps[v] = true
	}
	// Only catalog ownership/support links are traversed. Arbitrary dependency
	// callers are never part of descriptor closure.
	visited := map[schema.CatalogObject]bool{}
	var add func(schema.CatalogObject)
	add = func(o schema.CatalogObject) {
		if !work.step() {
			return
		}
		o = watchedObject(o)
		if visited[o] {
			return
		}
		if !work.charge(4096 + 8*watchedObjectBytes(o)) {
			return
		}
		visited[o] = true
		if o.Name == "" {
			reason = "mapping"
			return
		}
		kind := catalog.ObjectKind(o.Kind)
		cat := catalog.MetadataKind("")
		switch o.Kind {
		case "domain":
			cat = catalog.MetadataDomains
			for _, r := range []watchedRouting{old, new} {
				for _, link := range r.reverse[o] {
					if !work.step() {
						return
					}
					if link.From.Kind == "procedure" || watchedObject(link.From).Kind == "relation" {
						add(link.From)
					}
				}
			}
		case "relation":
			kind = catalog.ObjectKindTable
			relations[o.Name] = true
			id := catalog.ObjectID{Kind: kind, Name: o.Name}
			cover(catalog.MetadataColumnsCurrent, id, false)
			cover(catalog.MetadataPrimaryKeys, id, false)
			cover(catalog.MetadataForeignKeys, id, false)
			for _, r := range []watchedRouting{old, new} {
				if isView(o, r) {
					cover(catalog.MetadataViews, catalog.ObjectID{Kind: catalog.ObjectKindView, Name: o.Name}, r.headers[watchedHeaderKey(o)].Family != "" && eligible(o, r) && r.headers[watchedHeaderKey(o)].Family == new.headers[watchedHeaderKey(o)].Family && isView(o, new))
					kind = catalog.ObjectKindView
				}
			}
			if eligible(o, old) || eligible(o, new) {
				cover(catalog.MetadataRelations, catalog.ObjectID{Kind: kind, Name: o.Name}, eligible(o, new) && new.headers[watchedHeaderKey(o)].Family != "")
			}
			for _, r := range []watchedRouting{old, new} {
				if h, ok := r.headers[watchedHeaderKey(o)]; ok && h.SystemFlag.Valid && h.SystemFlag.Int64 != 0 {
					cover(catalog.MetadataSystemRelations, id, r.headers[watchedHeaderKey(o)].Digest == new.headers[watchedHeaderKey(o)].Digest)
					descriptors[id] = true
				}
			}
			if eligible(o, old) || eligible(o, new) {
				descriptors[catalog.ObjectID{Kind: kind, Name: o.Name}] = true
			}
			if _, was := old.headers[watchedHeaderKey(o)]; was {
				if _, exists := new.headers[watchedHeaderKey(o)]; !exists {
					deleted[catalog.ObjectID{Kind: kind, Name: o.Name}] = true
				}
			}
			addDep(o, old)
			addDep(o, new)
			return
		case "procedure":
			cat = catalog.MetadataProcedures
		case "trigger":
			cat = catalog.MetadataTriggers
		case "index":
			cat = catalog.MetadataIndexes
		case "generator":
			cat = catalog.MetadataGenerators
		case "exception":
			cat = catalog.MetadataExceptions
		case "function":
			cat = catalog.MetadataFunctions
		case "constraint":
			found := false
			for _, r := range []watchedRouting{old, new} {
				for _, link := range r.forward[o] {
					if !work.step() {
						return
					}
					if watchedObject(link.To).Kind == "relation" {
						found = true
					}
					if link.To.Kind == "constraint" || link.To.Kind == "index" || link.To.Kind == "trigger" || watchedObject(link.To).Kind == "relation" {
						add(link.To)
					}
				}
			}
			if !found {
				reason = "mapping"
			}
			return
		case "character set", "collation", "column collation":
			for _, r := range []watchedRouting{old, new} {
				for _, link := range r.reverse[o] {
					if !work.step() {
						return
					}
					add(link.From)
				}
			}
			return
		default:
			reason = "mapping"
			return
		}
		id := catalog.ObjectID{Kind: kind, Name: o.Name}
		if eligible(o, old) || eligible(o, new) {
			cover(cat, id, eligible(o, new) && new.headers[watchedHeaderKey(o)].Family != "")
			descriptors[id] = true
			if _, exists := old.headers[watchedHeaderKey(o)]; exists {
				if _, now := new.headers[watchedHeaderKey(o)]; !now || !eligible(o, new) {
					deleted[id] = true
				}
			}
		}
		addDep(o, old)
		addDep(o, new)
		if o.Kind == "index" || o.Kind == "trigger" {
			for _, r := range []watchedRouting{old, new} {
				for _, link := range r.forward[o] {
					if !work.step() {
						return
					}
					if watchedObject(link.To).Kind == "relation" || link.Field == "FOREIGN_KEY" {
						add(link.To)
					}
				}
				if o.Kind == "trigger" {
					for _, link := range r.reverse[o] {
						if !work.step() {
							return
						}
						if link.Field == "CHECK" {
							add(link.From)
						}
					}
				}
			}
		}
	}
	change := func(row schema.FingerprintRecord, other *schema.FingerprintRecord) {
		if !work.step() {
			return
		}
		if reason == "visibility" {
			return
		}
		switch row.Family {
		case "RDB$USER_PRIVILEGES", "RDB$ROLES", "RDB$SECURITY_CLASSES", "RDB$DATABASE":
			reason = "visibility"
			return
		}
		if other != nil {
			if watchedHeaderFamily(row.Family) && watchedHeaderCategory(row) != watchedHeaderCategory(*other) {
				reason = "mapping"
				return
			}
			securityLinks := func(r schema.FingerprintRecord) map[schema.CatalogLink]bool {
				m := map[schema.CatalogLink]bool{}
				for _, l := range r.Links {
					if !work.step() {
						return nil
					}
					if l.To.Kind == "principal" || l.To.Kind == "security class" {
						if !work.charge(512 + 2*watchedLinkBytes(l)) {
							return nil
						}
						m[l] = true
					}
				}
				return m
			}
			a, b := securityLinks(row), securityLinks(*other)
			if len(a) != len(b) {
				reason = "visibility"
				return
			}
			for l := range a {
				if !work.step() {
					return
				}
				if !b[l] {
					reason = "visibility"
					return
				}
			}
		}
		if row.Family == "RDB$DEPENDENCIES" {
			for _, o := range row.Owners {
				if !work.step() {
					return
				}
				if !o.NativeType.Valid {
					reason = "mapping"
					return
				}
				addDep(o, old)
				addDep(o, new)
			}
			return
		}
		if len(row.Owners) == 0 {
			reason = "mapping"
			return
		}
		for _, o := range row.Owners {
			if !work.step() {
				return
			}
			add(o)
		}
		// A changed relation header owns its complete index/trigger groups, including
		// empty groups. Child/domain changes do not reopen unrelated source groups.
		if row.Family == "RDB$RELATIONS" {
			o := row.Owners[0]
			id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: o.Name}
			cover(catalog.MetadataIndexes, id, false)
			cover(catalog.MetadataTriggers, id, false)
		}
	}
	current.VisitRecords(func(row schema.FingerprintRecord) bool {
		if !work.step() {
			return false
		}
		r, ok := previous.LookupRecord(row.Family, row.Key)
		if !ok {
			change(row, nil)
		} else if row.Digest != r.Digest || row.Count != r.Count {
			change(row, &r)
			change(r, &row)
		}
		return true
	})
	previous.VisitRecords(func(row schema.FingerprintRecord) bool {
		if !work.step() {
			return false
		}
		if _, ok := current.LookupRecord(row.Family, row.Key); !ok {
			change(row, nil)
		}
		return true
	})
	if reason != "" {
		return full(reason)
	}
	if len(descriptors) > 100 {
		return full("volume")
	}
	sel.Affected = len(descriptors)
	for k, m := range owners {
		for id := range m {
			if !work.step() {
				return full("mapping")
			}
			sel.Owners[k] = append(sel.Owners[k], id)
		}
		sort.Slice(sel.Owners[k], func(i, j int) bool { return sel.Owners[k][i].Key() < sel.Owners[k][j].Key() })
	}
	for id := range deleted {
		if !work.step() {
			return full("mapping")
		}
		sel.Deleted = append(sel.Deleted, id)
	}
	// Relation-group coverage includes every eligible candidate index/trigger
	// header, not just a named top-level owner. Validate these after all batches.
	for _, routing := range []watchedRouting{old, new} {
		for _, header := range routing.headers {
			if !work.step() {
				return full("mapping")
			}
			object := header.Owners[0]
			kind := catalog.MetadataKind("")
			switch object.Kind {
			case "index":
				kind = catalog.MetadataIndexes
			case "trigger":
				kind = catalog.MetadataTriggers
			default:
				continue
			}
			if !eligible(object, routing) {
				continue
			}
			id := catalog.ObjectID{Kind: catalog.ObjectKind(object.Kind), Name: object.Name}
			covered := owners[kind][id]
			for _, link := range header.Links {
				if !work.step() {
					return full("mapping")
				}
				if watchedObject(link.To).Kind == "relation" {
					for owner := range owners[kind] {
						if !work.step() {
							return full("mapping")
						}
						if (owner.Kind == catalog.ObjectKindTable || owner.Kind == catalog.ObjectKindView) && owner.Name == link.To.Name {
							covered = true
						}
					}
				}
			}
			if covered {
				descriptors[id] = true
				addDep(object, routing)
				if candidate, exists := new.headers[watchedHeaderKey(object)]; exists && candidate.Digest == header.Digest && eligible(object, new) {
					if sel.expected[kind] == nil {
						sel.expected[kind] = map[catalog.ObjectID]bool{}
					}
					sel.expected[kind][id] = true
				}
			}
		}
	}
	if reason != "" {
		return full(reason)
	}
	if len(descriptors) > 100 {
		return full("volume")
	}
	sel.Affected = len(descriptors)
	// Do not expand Owners with those validation-only headers: existing group
	// owners already cover them. Child counts detect a lost signature row even
	// when the corresponding header is still readable.
	sel.expectedChildren = map[catalog.MetadataKind]map[catalog.ObjectID]int{}
	for _, kind := range []catalog.MetadataKind{catalog.MetadataProcedures, catalog.MetadataFunctions} {
		for _, id := range sel.Owners[kind] {
			if !work.step() {
				return full("mapping")
			}
			if eligible(schema.CatalogObject{Kind: string(id.Kind), Name: id.Name}, new) && new.headers[watchedHeaderKey(schema.CatalogObject{Kind: string(id.Kind), Name: id.Name})].Family != "" {
				if sel.expectedChildren[kind] == nil {
					sel.expectedChildren[kind] = map[catalog.ObjectID]int{}
				}
				sel.expectedChildren[kind][id] = 0
			}
		}
	}
	current.VisitRecords(func(row schema.FingerprintRecord) bool {
		if !work.step() {
			return false
		}
		kind := catalog.MetadataKind("")
		switch row.Family {
		case "RDB$PROCEDURE_PARAMETERS":
			kind = catalog.MetadataProcedures
		case "RDB$FUNCTION_ARGUMENTS":
			kind = catalog.MetadataFunctions
		default:
			return true
		}
		for _, object := range row.Owners {
			if !work.step() {
				return false
			}
			id := catalog.ObjectID{Kind: catalog.ObjectKind(object.Kind), Name: object.Name}
			if owners[kind][id] && eligible(object, new) {
				if sel.expectedChildren[kind] == nil {
					sel.expectedChildren[kind] = map[catalog.ObjectID]int{}
				}
				sel.expectedChildren[kind][id] += int(row.Count)
				if sel.expected[kind] == nil {
					sel.expected[kind] = map[catalog.ObjectID]bool{}
				}
				sel.expected[kind][id] = true
			}
		}
		return true
	})
	sort.Slice(sel.Deleted, func(i, j int) bool { return sel.Deleted[i].Key() < sel.Deleted[j].Key() })
	// Candidate edges are complete in the zero-BLOB manifest. Normalize only the
	// affected outgoing groups, using candidate check/column ownership routes.
	physicallyGone := func(ref catalog.DependencyRef) bool {
		for id := range deleted {
			if !work.step() {
				return false
			}
			if id.Kind == ref.Kind && id.Name == ref.Name {
				return true
			}
			if (id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView) && ((ref.Parent == id.Name) || ((ref.Kind == catalog.ObjectKindTable || ref.Kind == catalog.ObjectKindView) && ref.Name == id.Name)) {
				return true
			}
		}
		return false
	}
	current.VisitRecords(func(row schema.FingerprintRecord) bool {
		if !work.step() {
			return false
		}
		if row.Family == "RDB$DEPENDENCIES" {
			for _, link := range row.Links {
				if !work.step() {
					return false
				}
				from, ok := ref(link.From, new)
				if !link.From.NativeType.Valid {
					ok = false
				}
				to, toOK := ref(link.To, new)
				if !link.To.NativeType.Valid {
					toOK = false
				}
				if !toOK {
					for owner := range deps {
						if !work.step() {
							return false
						}
						if owner.Name == link.To.Name || to.Parent != "" && owner.Name == to.Parent {
							reason = "mapping"
						}
					}
				}
				if ok && physicallyGone(from) || toOK && physicallyGone(to) {
					reason = "mapping"
					continue
				}
				if !ok {
					for owner := range deps {
						if !work.step() {
							return false
						}
						if owner.Name == link.From.Name {
							reason = "mapping"
						}
					}
					continue
				}
				if deps[from] {
					if !toOK {
						reason = "mapping"
						continue
					}
					if !work.charge(1024 + 4*watchedLinkBytes(link)) {
						return false
					}
					sel.dependencyEdges = append(sel.dependencyEdges, catalog.DependencyEdge{From: from, To: to, Field: link.Field})
				}
			}
		}
		if row.Family == "RDB$CHECK_CONSTRAINTS" {
			for _, owner := range row.Owners {
				if !work.step() {
					return false
				}
				for _, link := range new.forward[watchedObject(owner)] {
					if !work.step() {
						return false
					}
					if watchedObject(link.To).Kind == "relation" && strings.TrimRight(link.Field, " ") == "CHECK" && relations[link.To.Name] {
						if !work.charge(int64(1024 + 4*(len(link.To.Name)+len(owner.Name)))) {
							return false
						}
						sel.checkNames[link.To.Name] = append(sel.checkNames[link.To.Name], owner.Name)
						deps[catalog.DependencyRef{Kind: catalog.ObjectKindConstraint, Name: owner.Name, Parent: link.To.Name}] = true
					}
				}
			}
		}
		return true
	})
	for relation := range relations {
		if !work.step() {
			return full("mapping")
		}
		if _, was := old.headers[watchedHeaderKey(schema.CatalogObject{Kind: "relation", Name: relation})]; was {
			if _, exists := new.headers[watchedHeaderKey(schema.CatalogObject{Kind: "relation", Name: relation})]; !exists {
				delete(sel.checkNames, relation)
				continue
			}
		}
		if _, ok := sel.checkNames[relation]; !ok {
			sel.checkNames[relation] = []string{}
		}
	}
	for d := range deps {
		if !work.step() {
			return full("mapping")
		}
		sel.DependencyOwners = append(sel.DependencyOwners, d)
	}
	sort.Slice(sel.DependencyOwners, func(i, j int) bool {
		a, b := sel.DependencyOwners[i], sel.DependencyOwners[j]
		return string(a.Kind)+a.Parent+a.Name < string(b.Kind)+b.Parent+b.Name
	})
	if reason != "" {
		return full(reason)
	}
	return sel, nil
}

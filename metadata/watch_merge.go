package metadata

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sqls-server/sqls/catalog"
)

func incomplete(reason string) error { return fmt.Errorf("%w: %s", ErrWatchIncomplete, reason) }

// MergeObjects builds a new generation from complete owner replacements. It
// never modifies base or promotes a failed/unknown category to ready. Unchanged
// descriptors remain shared; replaced descriptors and changed projections are
// copied. A reader must supply exact maps, not folded partial fragments.
func MergeObjects(ctx context.Context, base *catalog.Cache, delta ObjectDelta) (*catalog.Cache, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if base == nil || delta.FullReason != "" || delta.Affected < 0 {
		return nil, incomplete("invalid incremental input")
	}
	for kind, owners := range delta.Owners {
		if !knownMetadataKind(kind) || kind == catalog.MetadataSchemas || kind == catalog.MetadataDependencies {
			return nil, incomplete("unsupported owner category")
		}
		patch, ok := delta.Patches[kind]
		if !ok || patch.Cache == nil || patch.Count < 0 || patch.Queries < 0 {
			return nil, incomplete("missing category replacement")
		}
		for k, state := range patch.Cache.Metadata {
			if k != kind || state != catalog.MetadataReady {
				return nil, incomplete("unsuccessful category replacement")
			}
		}
		seen := make(map[string]bool)
		for _, id := range owners {
			if !ownerKind(kind, id) || !validID(id) || seen[id.Key()] {
				return nil, incomplete("invalid category owner")
			}
			seen[id.Key()] = true
		}
		if err := validateWatchFragment(kind, patch.Cache); err != nil {
			return nil, err
		}
	}
	for kind := range delta.Patches {
		if _, ok := delta.Owners[kind]; !ok {
			return nil, incomplete("patch has no owner coverage")
		}
	}
	deleted := make(map[catalog.ObjectID]bool, len(delta.Deleted))
	for _, id := range delta.Deleted {
		if !validID(id) || !descriptorKind(id.Kind) {
			return nil, incomplete("invalid deleted identity")
		}
		deleted[id] = true
	}
	// A covered top-level descriptor with no row is a known removal. Discover
	// relation removals before pruning their index/trigger groups, not afterwards.
	for kind, owners := range delta.Owners {
		for _, id := range owners {
			if watchOwnerAbsent(kind, delta.Patches[kind].Cache, id) {
				deleted[id] = true
			}
		}
	}
	next := *base
	next.Catalog = cloneCatalogHeader(base.Catalog)
	if err := mergeWatchCatalog(&next, base, delta, deleted); err != nil {
		return nil, err
	}
	// Empty covered groups remove their old descriptor identities as well. Their
	// incident dependency edges must disappear even without an explicit Deleted.
	if err := recordRemovedCatalog(base.Catalog, next.Catalog, deleted); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := mergeWatchRelations(&next, base, delta, deleted); err != nil {
		return nil, err
	}
	if err := mergeWatchColumns(&next, base, delta, deleted); err != nil {
		return nil, err
	}
	if err := mergeWatchForeignKeys(&next, base, delta, deleted); err != nil {
		return nil, err
	}
	if err := mergeWatchDependencies(&next, base, delta, deleted); err != nil {
		return nil, err
	}
	if base.Catalog == nil && next.Catalog.Objects == nil {
		next.Catalog = nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &next, nil
}

func watchOwnerAbsent(k catalog.MetadataKind, p *catalog.Cache, id catalog.ObjectID) bool {
	if k == catalog.MetadataRelations {
		for _, name := range p.SchemaTables[id.Schema] {
			if name == id.Name {
				return false
			}
		}
		return true
	}
	c := p.Catalog
	if c == nil {
		return false
	}
	switch k {
	case catalog.MetadataViews:
		return c.ViewsByID != nil && c.ViewsByID[id.Key()] == nil
	case catalog.MetadataProcedures:
		return c.ProceduresByID != nil && c.ProceduresByID[id.Key()] == nil
	case catalog.MetadataGenerators:
		return c.GeneratorsByID != nil && c.GeneratorsByID[id.Key()] == nil
	case catalog.MetadataDomains:
		return c.DomainsByID != nil && c.DomainsByID[id.Key()] == nil
	case catalog.MetadataFunctions:
		return c.FunctionsByID != nil && c.FunctionsByID[id.Key()] == nil
	case catalog.MetadataIndexes:
		return id.Kind == catalog.ObjectKindIndex && c.IndexesByID != nil && c.IndexesByID[id.Key()] == nil
	case catalog.MetadataTriggers:
		return id.Kind == catalog.ObjectKindTrigger && c.TriggersByID != nil && c.TriggersByID[id.Key()] == nil
	case catalog.MetadataSystemRelations:
		return c.SystemRelationsByID != nil && c.SystemRelationsByID[id.Key()] == nil
	case catalog.MetadataExceptions:
		return c.ExceptionsByID != nil && c.ExceptionsByID[id.Key()] == nil
	}
	return false
}

func descriptorKind(k catalog.ObjectKind) bool {
	switch k {
	case catalog.ObjectKindTable, catalog.ObjectKindView, catalog.ObjectKindProcedure, catalog.ObjectKindTrigger, catalog.ObjectKindDomain, catalog.ObjectKindIndex, catalog.ObjectKindGenerator, catalog.ObjectKindFunction, catalog.ObjectKindException:
		return true
	}
	return false
}
func validID(id catalog.ObjectID) bool { return id.Name != "" && id.Catalog == "" && id.Owner == "" }
func ownerKind(k catalog.MetadataKind, id catalog.ObjectID) bool {
	switch k {
	case catalog.MetadataRelations:
		return id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView
	case catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll, catalog.MetadataPrimaryKeys, catalog.MetadataForeignKeys, catalog.MetadataSystemRelations:
		return id.Kind == catalog.ObjectKindTable
	case catalog.MetadataViews:
		return id.Kind == catalog.ObjectKindView
	case catalog.MetadataProcedures:
		return id.Kind == catalog.ObjectKindProcedure
	case catalog.MetadataDomains:
		return id.Kind == catalog.ObjectKindDomain
	case catalog.MetadataGenerators:
		return id.Kind == catalog.ObjectKindGenerator
	case catalog.MetadataFunctions:
		return id.Kind == catalog.ObjectKindFunction
	case catalog.MetadataIndexes:
		return id.Kind == catalog.ObjectKindIndex || id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView
	case catalog.MetadataTriggers:
		return id.Kind == catalog.ObjectKindTrigger || id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView
	case catalog.MetadataExceptions:
		return id.Kind == catalog.ObjectKindException
	}
	return false
}

// Empty legacy maps are permitted (NewCache supplies them), but a fragment may
// not smuggle another category's rows alongside its authoritative replacement.
func validateWatchFragment(k catalog.MetadataKind, c *catalog.Cache) error {
	if len(c.Schemas) > 0 || c.DefaultSchema != "" || c.Dependencies != nil {
		return incomplete("unexpected category data")
	}
	if len(c.SchemaTables) > 0 && k != catalog.MetadataRelations {
		return incomplete("unexpected relation rows")
	}
	if (len(c.ColumnsByID) > 0 || len(c.ColumnsWithParent) > 0) && k != catalog.MetadataColumnsAll && k != catalog.MetadataColumnsCurrent {
		return incomplete("unexpected column rows")
	}
	if (len(c.PrimaryKeysByID) > 0 || len(c.PrimaryKeyColumns) > 0) && k != catalog.MetadataPrimaryKeys {
		return incomplete("unexpected key rows")
	}
	if len(c.ForeignKeys) > 0 && k != catalog.MetadataForeignKeys {
		return incomplete("unexpected FK rows")
	}
	if cc := c.Catalog; cc != nil {
		if (len(cc.ViewsByID) > 0 || len(cc.Views) > 0) && k != catalog.MetadataViews {
			return incomplete("unexpected view rows")
		}
		if (len(cc.ProceduresByID) > 0 || len(cc.Procedures) > 0) && k != catalog.MetadataProcedures {
			return incomplete("unexpected procedure rows")
		}
		if (len(cc.GeneratorsByID) > 0 || len(cc.Generators) > 0) && k != catalog.MetadataGenerators {
			return incomplete("unexpected generator rows")
		}
		if (len(cc.DomainsByID) > 0 || len(cc.Domains) > 0) && k != catalog.MetadataDomains {
			return incomplete("unexpected domain rows")
		}
		if (len(cc.FunctionsByID) > 0 || len(cc.Functions) > 0) && k != catalog.MetadataFunctions {
			return incomplete("unexpected function rows")
		}
		if (len(cc.IndexesByID) > 0 || len(cc.Indexes) > 0 || len(cc.IndexesByTable) > 0) && k != catalog.MetadataIndexes {
			return incomplete("unexpected index rows")
		}
		if (len(cc.TriggersByID) > 0 || len(cc.Triggers) > 0 || len(cc.TriggersByTable) > 0) && k != catalog.MetadataTriggers {
			return incomplete("unexpected trigger rows")
		}
		if len(cc.SystemRelationsByID) > 0 && k != catalog.MetadataSystemRelations {
			return incomplete("unexpected system rows")
		}
		if len(cc.ExceptionsByID) > 0 && k != catalog.MetadataExceptions {
			return incomplete("unexpected exception rows")
		}
		for _, v := range cc.ViewsByID {
			if v == nil {
				return incomplete("nil view")
			}
			for _, col := range v.Columns {
				if col == nil || col.Name == "" || col.Table != v.Name || col.Schema != v.Schema {
					return incomplete("unknown view column")
				}
			}
		}
		for _, v := range cc.ProceduresByID {
			if v == nil {
				return incomplete("nil procedure")
			}
			for _, params := range [][]*catalog.ProcedureParameterDesc{v.InputParameters, v.OutputParameters} {
				for _, param := range params {
					if param == nil || param.Name == "" || param.Position < 0 {
						return incomplete("unknown procedure parameter")
					}
				}
			}
		}
		for _, v := range cc.FunctionsByID {
			if v == nil {
				return incomplete("nil function")
			}
			for _, a := range v.Arguments {
				if a == nil || a.FunctionName != v.Name {
					return incomplete("unknown function argument")
				}
			}
		}
		for key, group := range cc.IndexesByTable {
			for _, v := range group {
				if v == nil || catalog.CatalogCacheKey(v.RelationName) != key || cc.IndexesByID[catalog.IndexObjectID(v.Schema, v.RelationName, v.Name).Key()] == nil {
					return incomplete("unknown index group row")
				}
			}
		}
		for key, group := range cc.TriggersByTable {
			for _, v := range group {
				if v == nil || !v.RelationName.Valid || catalog.CatalogCacheKey(v.RelationName.String) != key || cc.TriggersByID[catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Schema: v.Schema, Name: v.Name}.Key()] == nil {
					return incomplete("unknown trigger group row")
				}
			}
		}
	}
	return nil
}

func recordRemoved[T any](before, after map[string]*T, deleted map[catalog.ObjectID]bool) error {
	for key := range before {
		if _, ok := after[key]; ok {
			continue
		}
		id, err := watchParseID(key)
		if err != nil {
			return err
		}
		deleted[id] = true
	}
	return nil
}
func recordRemovedCatalog(before, after *catalog.CatalogCache, deleted map[catalog.ObjectID]bool) error {
	if before == nil {
		return nil
	}
	if err := recordRemoved(before.ViewsByID, after.ViewsByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.ProceduresByID, after.ProceduresByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.DomainsByID, after.DomainsByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.GeneratorsByID, after.GeneratorsByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.FunctionsByID, after.FunctionsByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.IndexesByID, after.IndexesByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.TriggersByID, after.TriggersByID, deleted); err != nil {
		return err
	}
	if err := recordRemoved(before.SystemRelationsByID, after.SystemRelationsByID, deleted); err != nil {
		return err
	}
	return recordRemoved(before.ExceptionsByID, after.ExceptionsByID, deleted)
}

func relationID(id catalog.ObjectID) catalog.ObjectID { id.Kind = catalog.ObjectKindTable; return id }
func relationDeleted(deleted map[catalog.ObjectID]bool, schema, name string) bool {
	return deleted[catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: schema, Name: name}] || deleted[catalog.ObjectID{Kind: catalog.ObjectKindView, Schema: schema, Name: name}]
}
func covered(owners []catalog.ObjectID, id catalog.ObjectID, relation string) bool {
	for _, owner := range owners {
		if owner == id || (relation != "" && (owner.Kind == catalog.ObjectKindTable || owner.Kind == catalog.ObjectKindView) && owner.Name == relation && owner.Schema == id.Schema) {
			return true
		}
	}
	return false
}

// replaceDescriptors checks both exact and optional legacy rows before copying.
func replaceDescriptors[T any](base, legacy, patch, patchLegacy map[string]*T, owners []catalog.ObjectID, supplied bool, deleted map[catalog.ObjectID]bool, identify func(*T) (catalog.ObjectID, string), copyValue func(*T) *T) (map[string]*T, map[string]*T, bool, error) {
	changed := supplied
	for _, v := range base {
		if v == nil {
			return nil, nil, false, incomplete("nil published descriptor")
		}
		id, rel := identify(v)
		if deleted[id] || relationDeleted(deleted, id.Schema, rel) {
			changed = true
		}
	}
	if base == nil {
		for _, v := range legacy {
			if v == nil {
				return nil, nil, false, incomplete("nil published folded descriptor")
			}
			id, rel := identify(v)
			if deleted[id] || relationDeleted(deleted, id.Schema, rel) {
				changed = true
			}
		}
	}
	if !changed {
		return base, legacy, false, nil
	}
	if base == nil && len(legacy) > 0 {
		return nil, nil, false, incomplete("published exact category unavailable")
	}
	if supplied && patch == nil {
		return nil, nil, false, incomplete("missing exact replacement map")
	}
	for key, v := range patch {
		if v == nil {
			return nil, nil, false, incomplete("nil replacement descriptor")
		}
		id, rel := identify(v)
		if key != id.Key() || !covered(owners, id, rel) || deleted[id] || relationDeleted(deleted, id.Schema, rel) {
			return nil, nil, false, incomplete("uncovered replacement descriptor")
		}
	}
	for _, v := range patchLegacy {
		if v == nil {
			return nil, nil, false, incomplete("nil folded descriptor")
		}
		id, rel := identify(v)
		if !covered(owners, id, rel) || patch[id.Key()] == nil {
			return nil, nil, false, incomplete("uncovered folded descriptor")
		}
	}
	exact := cloneMap(base)
	for key, v := range exact {
		id, rel := identify(v)
		if covered(owners, id, rel) || deleted[id] || relationDeleted(deleted, id.Schema, rel) {
			delete(exact, key)
		}
	}
	for key, v := range patch {
		exact[key] = copyValue(v)
	}
	values := make([]*T, 0, len(exact))
	for _, v := range exact {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool {
		a, _ := identify(values[i])
		b, _ := identify(values[j])
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Schema < b.Schema
	})
	folded := make(map[string]*T, len(exact))
	for _, v := range values {
		id, _ := identify(v)
		folded[catalog.CatalogCacheKey(id.Name)] = v
	}
	return exact, folded, true, nil
}

func copyScalar[T any](v *T) *T { x := *v; return &x }
func copyFacts(v *catalog.ColumnTypeFacts) *catalog.ColumnTypeFacts {
	if v == nil {
		return nil
	}
	return copyScalar(v)
}
func copyColumns(values []*catalog.ColumnDesc) []*catalog.ColumnDesc {
	result := make([]*catalog.ColumnDesc, len(values))
	for i, v := range values {
		if v != nil {
			result[i] = copyScalar(v)
			result[i].Facts = copyFacts(v.Facts)
		}
	}
	return result
}
func copyProcedure(v *catalog.ProcedureDesc) *catalog.ProcedureDesc {
	x := *v
	copyParams := func(values []*catalog.ProcedureParameterDesc) []*catalog.ProcedureParameterDesc {
		if values == nil {
			return nil
		}
		r := make([]*catalog.ProcedureParameterDesc, len(values))
		for i, p := range values {
			if p != nil {
				r[i] = copyScalar(p)
				r[i].Facts = copyFacts(p.Facts)
			}
		}
		return r
	}
	x.InputParameters = copyParams(v.InputParameters)
	x.OutputParameters = copyParams(v.OutputParameters)
	return &x
}

func mergeWatchCatalog(next, base *catalog.Cache, d ObjectDelta, deleted map[catalog.ObjectID]bool) error {
	b := cloneCatalogHeader(base.Catalog)
	n := next.Catalog
	changed := false
	// Typed maps preserve unchanged descriptor pointers. The small repetitive
	// dispatch keeps engine-neutral models statically checked without reflection.
	for _, k := range []catalog.MetadataKind{catalog.MetadataViews, catalog.MetadataProcedures, catalog.MetadataGenerators, catalog.MetadataDomains, catalog.MetadataFunctions, catalog.MetadataIndexes, catalog.MetadataTriggers, catalog.MetadataSystemRelations, catalog.MetadataExceptions} {
		p := &catalog.CatalogCache{}
		patch, has := d.Patches[k]
		if has {
			if patch.Cache.Catalog == nil {
				return incomplete("missing catalog replacement")
			}
			p = patch.Cache.Catalog
		}
		owners := d.Owners[k]
		var touch bool
		var err error
		switch k {
		case catalog.MetadataViews:
			n.ViewsByID, n.Views, touch, err = replaceDescriptors(b.ViewsByID, b.Views, p.ViewsByID, p.Views, owners, has, deleted, func(v *catalog.ViewDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindView, Schema: v.Schema, Name: v.Name}, ""
			}, func(v *catalog.ViewDesc) *catalog.ViewDesc { x := *v; x.Columns = copyColumns(v.Columns); return &x })
		case catalog.MetadataProcedures:
			n.ProceduresByID, n.Procedures, touch, err = replaceDescriptors(b.ProceduresByID, b.Procedures, p.ProceduresByID, p.Procedures, owners, has, deleted, func(v *catalog.ProcedureDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Schema: v.Schema, Name: v.Name}, ""
			}, copyProcedure)
		case catalog.MetadataGenerators:
			n.GeneratorsByID, n.Generators, touch, err = replaceDescriptors(b.GeneratorsByID, b.Generators, p.GeneratorsByID, p.Generators, owners, has, deleted, func(v *catalog.GeneratorDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindGenerator, Schema: v.Schema, Name: v.Name}, ""
			}, copyScalar[catalog.GeneratorDesc])
		case catalog.MetadataDomains:
			n.DomainsByID, n.Domains, touch, err = replaceDescriptors(b.DomainsByID, b.Domains, p.DomainsByID, p.Domains, owners, has, deleted, func(v *catalog.DomainDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindDomain, Schema: v.Schema, Name: v.Name}, ""
			}, func(v *catalog.DomainDesc) *catalog.DomainDesc { x := *v; x.Facts = copyFacts(v.Facts); return &x })
		case catalog.MetadataFunctions:
			n.FunctionsByID, n.Functions, touch, err = replaceDescriptors(b.FunctionsByID, b.Functions, p.FunctionsByID, p.Functions, owners, has, deleted, func(v *catalog.FunctionDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindFunction, Schema: v.Schema, Name: v.Name}, ""
			}, func(v *catalog.FunctionDesc) *catalog.FunctionDesc {
				x := *v
				if v.Arguments != nil {
					x.Arguments = make([]*catalog.FunctionArgumentDesc, len(v.Arguments))
					for i, a := range v.Arguments {
						if a != nil {
							x.Arguments[i] = copyScalar(a)
						}
					}
				}
				return &x
			})
		case catalog.MetadataIndexes:
			n.IndexesByID, n.Indexes, touch, err = replaceDescriptors(b.IndexesByID, b.Indexes, p.IndexesByID, p.Indexes, owners, has, deleted, func(v *catalog.IndexDesc) (catalog.ObjectID, string) {
				return catalog.IndexObjectID(v.Schema, v.RelationName, v.Name), v.RelationName
			}, func(v *catalog.IndexDesc) *catalog.IndexDesc {
				x := *v
				x.Columns = append([]string(nil), v.Columns...)
				return &x
			})
			if touch {
				n.IndexesByTable = indexGroups(n.IndexesByID)
			}
		case catalog.MetadataTriggers:
			n.TriggersByID, n.Triggers, touch, err = replaceDescriptors(b.TriggersByID, b.Triggers, p.TriggersByID, p.Triggers, owners, has, deleted, func(v *catalog.TriggerDesc) (catalog.ObjectID, string) {
				rel := ""
				if v.RelationName.Valid {
					rel = v.RelationName.String
				}
				return catalog.ObjectID{Kind: catalog.ObjectKindTrigger, Schema: v.Schema, Name: v.Name}, rel
			}, copyScalar[catalog.TriggerDesc])
			if touch {
				n.TriggersByTable = triggerGroups(n.Triggers)
			}
		case catalog.MetadataSystemRelations:
			n.SystemRelationsByID, _, touch, err = replaceDescriptors(b.SystemRelationsByID, nil, p.SystemRelationsByID, nil, owners, has, deleted, func(v *catalog.SystemRelationDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: v.Schema, Name: v.Name}, ""
			}, copyScalar[catalog.SystemRelationDesc])
		case catalog.MetadataExceptions:
			n.ExceptionsByID, _, touch, err = replaceDescriptors(b.ExceptionsByID, nil, p.ExceptionsByID, nil, owners, has, deleted, func(v *catalog.ExceptionDesc) (catalog.ObjectID, string) {
				return catalog.ObjectID{Kind: catalog.ObjectKindException, Schema: v.Schema, Name: v.Name}, ""
			}, copyScalar[catalog.ExceptionDesc])
		}
		if err != nil {
			return err
		}
		changed = changed || touch
	}
	if changed {
		if err := watchRebuildObjects(n); err != nil {
			return err
		}
	}
	return nil
}

func watchKeys[T any](m map[string]*T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}
func watchRebuildObjects(c *catalog.CatalogCache) error {
	if (c.ViewsByID == nil && len(c.Views) > 0) || (c.ProceduresByID == nil && len(c.Procedures) > 0) || (c.GeneratorsByID == nil && len(c.Generators) > 0) || (c.DomainsByID == nil && len(c.Domains) > 0) || (c.FunctionsByID == nil && len(c.Functions) > 0) || (c.IndexesByID == nil && len(c.Indexes) > 0) || (c.TriggersByID == nil && len(c.Triggers) > 0) {
		return incomplete("published exact object index unavailable")
	}
	var keys []string
	for _, group := range [][]string{watchKeys(c.ViewsByID), watchKeys(c.ProceduresByID), watchKeys(c.GeneratorsByID), watchKeys(c.DomainsByID), watchKeys(c.FunctionsByID), watchKeys(c.IndexesByID), watchKeys(c.TriggersByID), watchKeys(c.SystemRelationsByID), watchKeys(c.ExceptionsByID)} {
		keys = append(keys, group...)
	}
	sort.Strings(keys)
	index := catalog.NewObjectIndex(nil)
	for _, key := range keys {
		id, err := watchParseID(key)
		if err != nil {
			return err
		}
		index.Insert(id)
	}
	c.Objects = index
	return nil
}

func mergeWatchRelations(next, base *catalog.Cache, d ObjectDelta, deleted map[catalog.ObjectID]bool) error {
	owners, has := d.Owners[catalog.MetadataRelations]
	touch := has
	for id := range deleted {
		if id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView {
			touch = true
		}
	}
	if !touch {
		return nil
	}
	next.SchemaTables = cloneStringSlices(base.SchemaTables)
	for schema, names := range next.SchemaTables {
		kept := names[:0]
		for _, name := range names {
			id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: schema, Name: name}
			if !relationDeleted(deleted, schema, name) && !covered(owners, id, "") && !covered(owners, catalog.ObjectID{Kind: catalog.ObjectKindView, Schema: schema, Name: name}, "") {
				kept = append(kept, name)
			}
		}
		next.SchemaTables[schema] = kept
	}
	if has {
		p := d.Patches[catalog.MetadataRelations].Cache
		if p.SchemaTables == nil {
			return incomplete("missing relation membership")
		}
		for schema, names := range p.SchemaTables {
			for _, name := range names {
				id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: schema, Name: name}
				if relationDeleted(deleted, schema, name) {
					return incomplete("deleted relation resurrected")
				}
				if !covered(owners, id, "") && !covered(owners, catalog.ObjectID{Kind: catalog.ObjectKindView, Schema: schema, Name: name}, "") {
					return incomplete("uncovered relation membership")
				}
				next.SchemaTables[schema] = append(next.SchemaTables[schema], name)
			}
		}
	}
	for schema, names := range next.SchemaTables {
		sort.Strings(names)
		for i := 1; i < len(names); i++ {
			if names[i] == names[i-1] {
				return incomplete("duplicate relation membership")
			}
		}
		next.SchemaTables[schema] = names
	}
	return nil
}

// Exact keys are opaque to consumers. Internally validating/decoding the five
// required length-prefixed components plus the optional parent component lets
// empty PK/column rows retain exact ownership and schema-scoped indexes retain
// their relation-qualified identity.
func watchParseID(key string) (catalog.ObjectID, error) {
	var parts [6]string
	rest := key
	parseComponent := func(i int) error {
		sep := strings.IndexByte(rest, ':')
		if sep < 1 {
			return incomplete("invalid exact identity")
		}
		n, err := strconv.Atoi(rest[:sep])
		rest = rest[sep+1:]
		if err != nil || n < 0 || n > len(rest) {
			return incomplete("invalid exact identity")
		}
		parts[i] = rest[:n]
		rest = rest[n:]
		return nil
	}
	for i := 0; i < 5; i++ {
		if err := parseComponent(i); err != nil {
			return catalog.ObjectID{}, err
		}
	}
	if rest != "" {
		if err := parseComponent(5); err != nil {
			return catalog.ObjectID{}, err
		}
	}
	id := catalog.ObjectID{Catalog: parts[0], Schema: parts[1], Owner: parts[2], Kind: catalog.ObjectKind(parts[3]), Name: parts[4], Parent: parts[5]}
	if rest != "" || !validID(id) || id.Key() != key {
		return catalog.ObjectID{}, incomplete("invalid exact identity")
	}
	return id, nil
}

func mergeWatchColumns(next, base *catalog.Cache, d ObjectDelta, deleted map[catalog.ObjectID]bool) error {
	changed := make(map[string]bool)
	columnsTouch, pkTouch := false, false
	for id := range deleted {
		if id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView {
			changed[relationID(id).Key()] = true
			columnsTouch = true
			pkTouch = true
		}
	}
	if _, ok := d.Owners[catalog.MetadataColumnsAll]; ok {
		if _, both := d.Owners[catalog.MetadataColumnsCurrent]; both {
			return incomplete("ambiguous column replacements")
		}
	}
	for _, k := range []catalog.MetadataKind{catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll, catalog.MetadataPrimaryKeys} {
		if _, has := d.Owners[k]; has {
			if k == catalog.MetadataPrimaryKeys {
				pkTouch = true
			} else {
				columnsTouch = true
			}
		}
		for _, id := range d.Owners[k] {
			changed[id.Key()] = true
			if k == catalog.MetadataPrimaryKeys {
				pkTouch = true
			} else {
				columnsTouch = true
			}
		}
	}
	if !columnsTouch && !pkTouch {
		return nil
	}
	if base.ColumnsByID == nil && (len(base.ColumnsWithParent) > 0 || base.ColumnsReady()) {
		return incomplete("published exact columns unavailable")
	}
	if base.PrimaryKeysByID == nil && (len(base.PrimaryKeyColumns) > 0 || base.MetadataReady(catalog.MetadataPrimaryKeys)) {
		return incomplete("published exact keys unavailable")
	}
	if base.ColumnsByID != nil {
		next.ColumnsByID = cloneMap(base.ColumnsByID)
	}
	if columnsTouch {
		for _, k := range []catalog.MetadataKind{catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll} {
			if _, has := d.Patches[k]; has && next.ColumnsByID == nil {
				next.ColumnsByID = make(map[string][]*catalog.ColumnDesc)
			}
		}
	}
	if base.PrimaryKeysByID != nil {
		next.PrimaryKeysByID = cloneMap(base.PrimaryKeysByID)
	}
	for key := range next.ColumnsByID {
		id, err := watchParseID(key)
		if err != nil {
			return err
		}
		if relationDeleted(deleted, id.Schema, id.Name) {
			delete(next.ColumnsByID, key)
		}
	}
	for key := range next.PrimaryKeysByID {
		id, err := watchParseID(key)
		if err != nil {
			return err
		}
		if relationDeleted(deleted, id.Schema, id.Name) {
			delete(next.PrimaryKeysByID, key)
		}
	}
	for _, k := range []catalog.MetadataKind{catalog.MetadataColumnsCurrent, catalog.MetadataColumnsAll} {
		owners, has := d.Owners[k]
		if !has {
			continue
		}
		p := d.Patches[k].Cache
		if p.ColumnsByID == nil {
			return incomplete("missing exact column replacement")
		}
		for _, id := range owners {
			delete(next.ColumnsByID, id.Key())
		}
		for key, cols := range p.ColumnsByID {
			id, err := watchParseID(key)
			if err != nil {
				return err
			}
			if id.Kind != catalog.ObjectKindTable || !covered(owners, id, "") {
				return incomplete("uncovered column owner")
			}
			if relationDeleted(deleted, id.Schema, id.Name) {
				if len(cols) > 0 {
					return incomplete("deleted columns resurrected")
				}
				continue
			}
			for _, c := range cols {
				if c == nil || c.Schema != id.Schema || c.Table != id.Name || c.Name == "" {
					return incomplete("invalid column row")
				}
			}
			next.ColumnsByID[key] = copyColumns(cols)
		}
		for _, cols := range p.ColumnsWithParent {
			for _, c := range cols {
				if c == nil || !covered(owners, catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: c.Schema, Name: c.Table}, "") {
					return incomplete("uncovered folded column")
				}
			}
		}
	}
	if owners, has := d.Owners[catalog.MetadataPrimaryKeys]; has {
		p := d.Patches[catalog.MetadataPrimaryKeys].Cache
		if p.PrimaryKeysByID == nil {
			return incomplete("missing exact key replacement")
		}
		if next.PrimaryKeysByID == nil {
			next.PrimaryKeysByID = make(map[string][]string)
		}
		for _, id := range owners {
			delete(next.PrimaryKeysByID, id.Key())
		}
		for key, names := range p.PrimaryKeysByID {
			id, err := watchParseID(key)
			if err != nil {
				return err
			}
			if id.Kind != catalog.ObjectKindTable || !covered(owners, id, "") {
				return incomplete("uncovered key owner")
			}
			if relationDeleted(deleted, id.Schema, id.Name) {
				if len(names) > 0 {
					return incomplete("deleted keys resurrected")
				}
				continue
			}
			seen := make(map[string]bool)
			for _, name := range names {
				if name == "" || seen[name] {
					return incomplete("invalid key segment")
				}
				seen[name] = true
			}
			next.PrimaryKeysByID[key] = append([]string(nil), names...)
		}
		for key := range p.PrimaryKeyColumns {
			found := false
			for _, id := range owners {
				if key == catalog.ColumnDatabaseKey(id.Schema, id.Name) {
					found = true
				}
			}
			if !found {
				return incomplete("uncovered folded key")
			}
		}
	}
	if pkTouch {
		next.PrimaryKeyColumns = make(map[string]map[string]struct{})
		for key, names := range next.PrimaryKeysByID {
			id, err := watchParseID(key)
			if err != nil {
				return err
			}
			fold := catalog.ColumnDatabaseKey(id.Schema, id.Name)
			if next.PrimaryKeyColumns[fold] == nil {
				next.PrimaryKeyColumns[fold] = make(map[string]struct{})
			}
			for _, name := range names {
				next.PrimaryKeyColumns[fold][name] = struct{}{}
			}
		}
	}
	// Ordinary full enrichment uses the folded PK union and EqualFold for
	// ColumnsByID as well as ColumnsWithParent. Exact routing and ordered keys
	// remain independent, but every surviving sibling in a touched folded bucket
	// must receive that same compatibility projection, including after a drop.
	foldedTouched := make(map[string]bool, len(changed))
	for key := range changed {
		id, err := watchParseID(key)
		if err != nil {
			return err
		}
		foldedTouched[catalog.ColumnDatabaseKey(id.Schema, id.Name)] = true
	}
	if base.MetadataReady(catalog.MetadataPrimaryKeys) {
		for key := range next.ColumnsByID {
			id, err := watchParseID(key)
			if err != nil {
				return err
			}
			if foldedTouched[catalog.ColumnDatabaseKey(id.Schema, id.Name)] {
				changed[key] = true
			}
		}
	}
	for key := range changed {
		cols := next.ColumnsByID[key]
		var copied []*catalog.ColumnDesc
		for i, c := range cols {
			if c == nil {
				return incomplete("nil published column")
			}
			keyValue := ""
			if base.MetadataReady(catalog.MetadataPrimaryKeys) {
				keyValue = "NO"
				for pk := range next.PrimaryKeyColumns[catalog.ColumnDatabaseKey(c.Schema, c.Table)] {
					if strings.EqualFold(pk, c.Name) {
						keyValue = "YES"
						break
					}
				}
			}
			if keyValue != c.Key {
				if copied == nil {
					copied = copyColumns(cols)
				}
				copied[i].Key = keyValue
			}
		}
		if copied != nil {
			next.ColumnsByID[key] = copied
		}
	}
	if columnsTouch || pkTouch {
		next.ColumnsWithParent = make(map[string][]*catalog.ColumnDesc)
		ids := make([]string, 0, len(next.ColumnsByID))
		for key := range next.ColumnsByID {
			ids = append(ids, key)
		}
		sort.Slice(ids, func(i, j int) bool {
			a, _ := watchParseID(ids[i])
			b, _ := watchParseID(ids[j])
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Schema < b.Schema
		})
		for _, key := range ids {
			for _, c := range next.ColumnsByID[key] {
				if c == nil {
					return incomplete("nil published column")
				}
				fold := catalog.ColumnDatabaseKey(c.Schema, c.Table)
				next.ColumnsWithParent[fold] = append(next.ColumnsWithParent[fold], c)
			}
		}
	}
	// Views own their column slice too; copy only descriptors whose columns changed.
	viewsChanged := false
	for key, view := range next.Catalog.ViewsByID {
		id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: view.Schema, Name: view.Name}
		if !changed[id.Key()] {
			continue
		}
		if !viewsChanged {
			next.Catalog.ViewsByID = cloneMap(next.Catalog.ViewsByID)
			next.Catalog.Views = cloneMap(next.Catalog.Views)
			viewsChanged = true
		}
		v := *view
		v.Columns = copyColumns(next.ColumnsByID[id.Key()])
		for _, c := range v.Columns {
			c.Key = "NO"
		}
		next.Catalog.ViewsByID[key] = &v
	}
	if viewsChanged {
		next.Catalog.Views = foldViews(next.Catalog.ViewsByID)
	}
	return nil
}

func foldViews(exact map[string]*catalog.ViewDesc) map[string]*catalog.ViewDesc {
	values := make([]*catalog.ViewDesc, 0, len(exact))
	for _, v := range exact {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Name != values[j].Name {
			return values[i].Name < values[j].Name
		}
		return values[i].Schema < values[j].Schema
	})
	folded := make(map[string]*catalog.ViewDesc)
	for _, v := range values {
		folded[catalog.CatalogCacheKey(v.Name)] = v
	}
	return folded
}

func mergeWatchForeignKeys(next, base *catalog.Cache, d ObjectDelta, deleted map[catalog.ObjectID]bool) error {
	owners, has := d.Owners[catalog.MetadataForeignKeys]
	touch := has
	for id := range deleted {
		if id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView {
			touch = true
		}
	}
	if !touch {
		return nil
	}
	collect := func(input map[string]map[string][]*catalog.ForeignKey) ([]*catalog.ForeignKey, error) {
		seen := make(map[*catalog.ForeignKey]bool)
		var keys []*catalog.ForeignKey
		for _, refs := range input {
			for _, values := range refs {
				for _, key := range values {
					if seen[key] {
						continue
					}
					seen[key] = true
					if catalog.ValidateForeignKey(key) != nil {
						return nil, incomplete("invalid FK pair")
					}
					first := (*key)[0]
					for _, pair := range *key {
						for end := 0; end < 2; end++ {
							if pair[end].Table == "" || pair[end].Name == "" || pair[end].Table != first[end].Table || pair[end].Schema != first[end].Schema {
								return nil, incomplete("inconsistent FK endpoint")
							}
						}
					}
					keys = append(keys, key)
				}
			}
		}
		return keys, nil
	}
	old, err := collect(base.ForeignKeys)
	if err != nil {
		return err
	}
	involved := func(key *catalog.ForeignKey) bool {
		for _, c := range (*key)[0] {
			id := catalog.ObjectID{Kind: catalog.ObjectKindTable, Schema: c.Schema, Name: c.Table}
			if covered(owners, id, "") {
				return true
			}
		}
		return false
	}
	gone := func(key *catalog.ForeignKey) bool {
		for _, c := range (*key)[0] {
			if relationDeleted(deleted, c.Schema, c.Table) {
				return true
			}
		}
		return false
	}
	keys := make([]*catalog.ForeignKey, 0, len(old))
	for _, key := range old {
		if !involved(key) && !gone(key) {
			keys = append(keys, key)
		}
	}
	if has {
		p := d.Patches[catalog.MetadataForeignKeys].Cache
		if p.ForeignKeys == nil {
			return incomplete("missing FK replacement")
		}
		incoming, err := collect(p.ForeignKeys)
		if err != nil {
			return err
		}
		for _, key := range incoming {
			if !involved(key) || gone(key) {
				return incomplete("uncovered FK pair")
			}
			copy := make(catalog.ForeignKey, len(*key))
			for i, pair := range *key {
				copy[i] = [2]*catalog.ColumnBase{copyScalar(pair[0]), copyScalar(pair[1])}
			}
			keys = append(keys, &copy)
		}
	}
	// Stable ordering independent of the folded maps' iteration order.
	sort.SliceStable(keys, func(i, j int) bool { return watchFKKey(keys[i]) < watchFKKey(keys[j]) })
	next.ForeignKeys = make(map[string]map[string][]*catalog.ForeignKey)
	for _, key := range keys {
		pair := (*key)[0]
		left := catalog.ColumnDatabaseKey(pair[0].Schema, pair[0].Table)
		right := catalog.ColumnDatabaseKey(pair[1].Schema, pair[1].Table)
		if next.ForeignKeys[left] == nil {
			next.ForeignKeys[left] = make(map[string][]*catalog.ForeignKey)
		}
		next.ForeignKeys[left][right] = append(next.ForeignKeys[left][right], key)
		if next.ForeignKeys[right] == nil {
			next.ForeignKeys[right] = make(map[string][]*catalog.ForeignKey)
		}
		next.ForeignKeys[right][left] = append(next.ForeignKeys[right][left], key)
	}
	return nil
}
func watchFKKey(key *catalog.ForeignKey) string {
	var b strings.Builder
	for _, pair := range *key {
		for _, c := range pair {
			b.WriteString(catalog.ObjectID{Schema: c.Schema, Kind: catalog.ObjectKindColumn, Owner: c.Table, Name: c.Name}.Key())
		}
	}
	return b.String()
}

func validRef(r catalog.DependencyRef) bool {
	if r.Name == "" {
		return false
	}
	if r.Kind == catalog.ObjectKindColumn || r.Kind == catalog.ObjectKindConstraint {
		return r.Parent != ""
	}
	return descriptorKind(r.Kind) && r.Parent == ""
}
func mergeWatchDependencies(next, base *catalog.Cache, d ObjectDelta, deleted map[catalog.ObjectID]bool) error {
	if len(d.DependencyOwners) == 0 && len(d.DependencyEdges) == 0 && len(d.CheckNames) == 0 && len(deleted) == 0 {
		return nil
	}
	if base.Dependencies == nil {
		if len(d.DependencyOwners) > 0 || len(d.DependencyEdges) > 0 || len(d.CheckNames) > 0 {
			return incomplete("published dependency graph unavailable")
		}
		return nil
	}
	for id := range deleted {
		if id.Schema != "" && (base.Dependencies.Len() > 0 || len(base.Dependencies.CheckNames()) > 0) {
			return incomplete("dependency graph has no schema identities")
		}
	}
	owners := make(map[catalog.DependencyRef]bool)
	for _, r := range d.DependencyOwners {
		if !validRef(r) || owners[r] {
			return incomplete("invalid dependency owner")
		}
		owners[r] = true
	}
	gone := func(r catalog.DependencyRef) bool {
		for id := range deleted {
			if id.Schema != "" {
				continue
			}
			if (r.Kind == id.Kind && r.Name == id.Name) || (r.Parent == id.Name && (id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView)) {
				return true
			}
			if id.Kind == catalog.ObjectKindView && r.Kind == catalog.ObjectKindTable && r.Name == id.Name {
				return true
			}
		}
		return false
	}
	edges := make([]catalog.DependencyEdge, 0, base.Dependencies.Len()+len(d.DependencyEdges))
	for _, edge := range base.Dependencies.Edges() {
		if !owners[edge.From] && !gone(edge.From) && !gone(edge.To) {
			edges = append(edges, edge)
		}
	}
	for _, edge := range d.DependencyEdges {
		if !validRef(edge.From) || !validRef(edge.To) || !owners[edge.From] || gone(edge.From) || gone(edge.To) {
			return incomplete("uncovered dependency edge")
		}
		edges = append(edges, edge)
	}
	checks := base.Dependencies.CheckNames()
	if checks == nil {
		checks = make(map[string][]string)
	}
	for id := range deleted {
		if id.Kind == catalog.ObjectKindTable || id.Kind == catalog.ObjectKindView {
			delete(checks, id.Name)
		}
	}
	for relation, names := range d.CheckNames {
		if relation == "" || relationDeleted(deleted, "", relation) {
			return incomplete("invalid check owner")
		}
		known := false
		for _, name := range next.SchemaTables[""] {
			if name == relation {
				known = true
				break
			}
		}
		if !known {
			_, known = checks[relation]
		}
		if !known {
			_, known = next.Catalog.SystemRelationsByID[catalog.ObjectID{Kind: catalog.ObjectKindTable, Name: relation}.Key()]
		}
		if !known {
			return incomplete("unknown check owner")
		}
		for _, name := range names {
			if name == "" {
				return incomplete("invalid check name")
			}
		}
		checks[relation] = append([]string(nil), names...)
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.From != b.From {
			return fmt.Sprint(a.From) < fmt.Sprint(b.From)
		}
		if a.To != b.To {
			return fmt.Sprint(a.To) < fmt.Sprint(b.To)
		}
		return a.Field < b.Field
	})
	next.Dependencies = catalog.NewDependencyGraph(edges, checks)
	return nil
}

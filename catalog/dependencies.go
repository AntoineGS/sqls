package catalog

import "sort"

const (
	ObjectKindColumn     ObjectKind = "column"
	ObjectKindConstraint ObjectKind = "constraint"
)

// DependencyRef identifies one end of a dependency. Parent names the owning
// relation of a column or constraint and is empty otherwise.
type DependencyRef struct {
	Kind   ObjectKind
	Name   string
	Parent string
}

func (r DependencyRef) key() string { return string(r.Kind) + "\x00" + r.Parent + "\x00" + r.Name }

// DependencyEdge records that From references To. Field is the referenced
// column of To, empty for a whole-object reference such as a procedure call.
type DependencyEdge struct {
	From  DependencyRef
	To    DependencyRef
	Field string
}

// DependencyGraph is an immutable set of dependency edges indexed in both
// directions, plus the check constraint names of each relation.
type DependencyGraph struct {
	edges  []DependencyEdge
	uses   map[string][]int32
	usedBy map[string][]int32
	checks map[string][]string
}

func NewDependencyGraph(edges []DependencyEdge, checks map[string][]string) *DependencyGraph {
	g := &DependencyGraph{
		edges:  make([]DependencyEdge, 0, len(edges)),
		uses:   make(map[string][]int32),
		usedBy: make(map[string][]int32),
		checks: make(map[string][]string, len(checks)),
	}
	seen := make(map[DependencyEdge]struct{}, len(edges))
	for _, edge := range edges {
		if _, dup := seen[edge]; dup {
			continue
		}
		seen[edge] = struct{}{}
		index := int32(len(g.edges))
		g.edges = append(g.edges, edge)
		g.uses[edge.From.key()] = append(g.uses[edge.From.key()], index)
		g.usedBy[edge.To.key()] = append(g.usedBy[edge.To.key()], index)
	}
	for relation, names := range checks {
		unique := append([]string(nil), names...)
		sort.Strings(unique)
		kept := unique[:0]
		for i, name := range unique {
			if i == 0 || name != unique[i-1] {
				kept = append(kept, name)
			}
		}
		g.checks[relation] = kept
	}
	return g
}

func (g *DependencyGraph) Len() int {
	if g == nil {
		return 0
	}
	return len(g.edges)
}

// Edges returns a copy of the graph's complete edge set.
func (g *DependencyGraph) Edges() []DependencyEdge {
	if g == nil {
		return nil
	}
	return append([]DependencyEdge(nil), g.edges...)
}

// CheckNames returns a deep copy of the complete relation/check-name projection.
func (g *DependencyGraph) CheckNames() map[string][]string {
	if g == nil {
		return nil
	}
	result := make(map[string][]string, len(g.checks))
	for relation, names := range g.checks {
		result[relation] = append([]string(nil), names...)
	}
	return result
}

// Uses returns the edges whose From is ref.
func (g *DependencyGraph) Uses(ref DependencyRef) []DependencyEdge {
	if g == nil {
		return nil
	}
	return g.collect(g.uses[ref.key()], "")
}

// UsedBy returns the edges whose To is ref, limited to field when it is set.
func (g *DependencyGraph) UsedBy(ref DependencyRef, field string) []DependencyEdge {
	if g == nil {
		return nil
	}
	return g.collect(g.usedBy[ref.key()], field)
}

func (g *DependencyGraph) collect(indexes []int32, field string) []DependencyEdge {
	var result []DependencyEdge
	for _, index := range indexes {
		edge := g.edges[index]
		if field != "" && edge.Field != field {
			continue
		}
		result = append(result, edge)
	}
	return result
}

// CheckConstraints returns relation's check constraint names, sorted.
func (g *DependencyGraph) CheckConstraints(relation string) []string {
	if g == nil || len(g.checks[relation]) == 0 {
		return nil
	}
	return append([]string(nil), g.checks[relation]...)
}

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/dialect"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

type gatedHoverProvider struct {
	name         string
	ddl          string
	native       string
	entered      chan struct{}
	release      chan struct{}
	done         chan struct{}
	completeOnce sync.Once
	loads        atomic.Int64
}

func newGatedHoverProvider(name, ddl string) *gatedHoverProvider {
	return &gatedHoverProvider{name: name, ddl: ddl, entered: make(chan struct{}, 1), release: make(chan struct{}), done: make(chan struct{})}
}

func (p *gatedHoverProvider) Plan() catalog.MetadataPlan {
	return catalog.MetadataPlan{Parallelism: 1, Jobs: []catalog.MetadataJob{{Kind: database.MetadataProcedures, Run: func(context.Context, *catalog.Cache) (catalog.MetadataPatch, error) {
		return catalog.MetadataPatch{Cache: &catalog.Cache{Catalog: &catalog.CatalogCache{Procedures: map[string]*catalog.ProcedureDesc{
			p.name: {Name: p.name, InputParameters: []*catalog.ProcedureParameterDesc{{Name: "P", Position: 0, Direction: catalog.ParameterInput, Type: "INTEGER"}}},
		}}}}, nil
	}}}}
}

func (p *gatedHoverProvider) DetailPlan(req metadata.Request) (metadata.DetailPlan, error) {
	if req.Object.Kind != catalog.ObjectKindProcedure || req.Object.Name != p.name {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	if req.Facet == "source" && p.native != "" {
		return metadata.DetailPlan{Requires: []catalog.MetadataKind{database.MetadataProcedures}, Coverage: req.Object.Key() + "\x00source",
			Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
				p.loads.Add(1)
				p.entered <- struct{}{}
				<-p.release
				p.completeOnce.Do(func() { close(p.done) })
				return metadata.Detail{Object: req.Object, Text: p.native, Provenance: "native-source"}, nil
			}}, nil
	}
	if req.Facet != "ddl" {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return metadata.DetailPlan{Requires: []catalog.MetadataKind{database.MetadataProcedures}, Coverage: req.Object.Key() + "\x00" + req.Facet,
		Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
			p.loads.Add(1)
			p.entered <- struct{}{}
			<-p.release // intentionally ignores the detached load context
			p.completeOnce.Do(func() { close(p.done) })
			if p.native != "" {
				return metadata.Detail{}, database.NewUnsupportedDDLError("procedure", p.name, "unsupported metadata")
			}
			return metadata.Detail{Object: req.Object, Text: p.ddl}, nil
		}}, nil
}

func (*gatedHoverProvider) Close() error { return nil }

func readyHoverLease(t *testing.T, s *Server, key string, provider *gatedHoverProvider) *metadata.Lease {
	t.Helper()
	lease, err := s.registry.Acquire(context.Background(), metadata.Key{Engine: "test", URL: key}, metadata.Scope{Principal: "test"}, func(context.Context) (metadata.Provider, error) { return provider, nil })
	if err != nil {
		t.Fatal("Acquire:", err)
	}
	t.Cleanup(lease.Release)
	if err := metadatatest.WaitReady(context.Background(), lease); err != nil {
		t.Fatal("WaitReady:", err)
	}
	cache := lease.Snapshot().Cache
	cache.Metadata[database.MetadataViews] = database.MetadataReady
	cache.Metadata[database.MetadataTriggers] = database.MetadataReady
	cache.Catalog.Views = map[string]*catalog.ViewDesc{}
	cache.Catalog.Triggers = map[string]*catalog.TriggerDesc{}
	cache.Catalog.ProceduresByID = map[string]*catalog.ProcedureDesc{(catalog.ObjectID{Kind: catalog.ObjectKindProcedure, Name: provider.name}).Key(): cache.Catalog.Procedures[provider.name]}
	cache.Catalog.ViewsByID = map[string]*catalog.ViewDesc{}
	cache.Catalog.TriggersByID = map[string]*catalog.TriggerDesc{}
	cache.Catalog.RebuildObjects(strings.ToUpper)
	return lease
}

func invokeHover(s *Server, text string) (*lsp.Hover, error) {
	params, _ := json.Marshal(lsp.HoverParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI}, Position: lsp.Position{Line: 0, Character: strings.LastIndex(text, "myproc") + 1},
	}})
	raw := json.RawMessage(params)
	value, err := s.handleTextDocumentHover(context.Background(), nil, &jsonrpc2.Request{Params: &raw})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	hover, ok := value.(*lsp.Hover)
	if !ok {
		return nil, fmt.Errorf("hover result %T, want *lsp.Hover", value)
	}
	return hover, nil
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestProductionHoverDetailTimeoutReturnsCachedSummary(t *testing.T) {
	oldTimeout := hoverDDLTimeout
	hoverDDLTimeout = 40 * time.Millisecond
	t.Cleanup(func() { hoverDDLTimeout = oldTimeout })
	s := newDefinitionServer(t)
	provider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END")
	lease := readyHoverLease(t, s, "hover-timeout", provider)
	const text = "execute procedure myproc"
	s.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, lease

	response := make(chan struct {
		hover *lsp.Hover
		err   error
	}, 1)
	go func() {
		h, err := invokeHover(s, text)
		response <- struct {
			hover *lsp.Hover
			err   error
		}{h, err}
	}()
	waitSignal(t, provider.entered, "provider Detail.Load start")
	var got struct {
		hover *lsp.Hover
		err   error
	}
	select {
	case got = <-response:
	case <-time.After(2 * time.Second):
		t.Fatal("production hover did not return after its detail budget")
	}
	if got.err != nil {
		t.Fatalf("hover returned JSON-RPC error on optional detail timeout: %v", got.err)
	}
	if got.hover == nil || !strings.Contains(got.hover.Contents.Value, "MYPROC") {
		t.Fatalf("hover = %#v, want cached procedure summary", got.hover)
	}
	if strings.Contains(got.hover.Contents.Value, "CREATE PROCEDURE") {
		t.Fatalf("timed-out hover unexpectedly includes DDL: %q", got.hover.Contents.Value)
	}

	close(provider.release)
	waitSignal(t, provider.done, "late provider load completion")
	if strings.Contains(got.hover.Contents.Value, "CREATE PROCEDURE") {
		t.Fatal("late detail mutated the already-returned hover")
	}
	if loads := provider.loads.Load(); loads != 1 {
		t.Fatalf("provider loads=%d, want exactly one", loads)
	}
	// The lease may retain its one completed shared Detail result; there is no
	// handler-side memo for a timed-out/late result to poison.
	next, err := invokeHover(s, text)
	if err != nil || next == nil || !strings.Contains(next.Contents.Value, "CREATE PROCEDURE") {
		t.Fatalf("subsequent hover = (%#v,%v), want completed lease detail", next, err)
	}
	if loads := provider.loads.Load(); loads != 1 {
		t.Fatalf("provider loads after cached hover=%d, want 1", loads)
	}
}

func TestProductionHoverRejectsResultAfterBindingChanges(t *testing.T) {
	oldTimeout := hoverDDLTimeout
	hoverDDLTimeout = time.Second
	t.Cleanup(func() { hoverDDLTimeout = oldTimeout })
	s := newDefinitionServer(t)
	oldProvider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END /* old database */")
	oldLease := readyHoverLease(t, s, "hover-old-binding", oldProvider)
	newProvider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END /* new database */")
	newLease := readyHoverLease(t, s, "hover-new-binding", newProvider)
	const text = "execute procedure myproc"
	s.files[testFileURI] = &File{Text: text, Version: 7, Revision: 11}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, oldLease

	response := make(chan error, 1)
	go func() { _, err := invokeHover(s, text); response <- err }()
	waitSignal(t, oldProvider.entered, "old binding Detail.Load start")
	s.stateMu.Lock()
	s.connGeneration = 2 // replacement binding; editor Version remains unchanged
	s.metaBoundGeneration = 2
	s.metaLease = newLease
	s.stateMu.Unlock()
	close(oldProvider.release)
	select {
	case err := <-response:
		if !errors.Is(err, language.ErrStale) {
			t.Fatalf("old in-flight hover err=%v, want language.ErrStale", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old in-flight hover did not settle")
	}
	waitSignal(t, oldProvider.done, "old provider load completion")

	s.stateMu.Lock()
	s.files[testFileURI].Version = 7
	s.files[testFileURI].Revision++
	s.stateMu.Unlock()
	close(newProvider.release) // the fresh lease's first detail returns immediately
	got, err := invokeHover(s, text)
	if err != nil {
		t.Fatalf("hover on replacement binding: %v", err)
	}
	if got == nil || !strings.Contains(got.Contents.Value, "new database") {
		t.Fatalf("new-binding hover=%#v, want new database DDL", got)
	}
}

func TestProductionDefinitionUsesSharedLeaseDetail(t *testing.T) {
	s := newDefinitionServer(t)
	provider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END")
	lease := readyHoverLease(t, s, "definition-production", provider)
	const text = "execute procedure myproc"
	s.files[testFileURI] = &File{Text: text, Version: 3, Revision: 8}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, lease
	params, _ := json.Marshal(lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI}, Position: lsp.Position{Line: 0, Character: 20}}})
	raw := json.RawMessage(params)
	response := make(chan struct {
		value interface{}
		err   error
	}, 1)
	go func() {
		value, err := s.handleDefinition(context.Background(), nil, &jsonrpc2.Request{Params: &raw})
		response <- struct {
			value interface{}
			err   error
		}{value, err}
	}()
	waitSignal(t, provider.entered, "definition provider Detail.Load start")
	close(provider.release)
	gotResponse := <-response
	value, err := gotResponse.value, gotResponse.err
	if err != nil {
		t.Fatalf("definition: %v", err)
	}
	got, ok := value.([]lsp.Location)
	if !ok || len(got) != 1 {
		t.Fatalf("definition = %#v, want one source location", value)
	}
	if !strings.Contains(got[0].URI, "file:") {
		t.Fatalf("URI = %q", got[0].URI)
	}
	if loads := provider.loads.Load(); loads != 1 {
		t.Fatalf("provider loads=%d, want one", loads)
	}
}

func invokeDefinition(s *Server, text string, character int) ([]lsp.Location, error) {
	params, _ := json.Marshal(lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI}, Position: lsp.Position{Line: 0, Character: character},
	}})
	raw := json.RawMessage(params)
	value, err := s.handleDefinition(context.Background(), nil, &jsonrpc2.Request{Params: &raw})
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	locations, ok := value.([]lsp.Location)
	if !ok {
		return nil, fmt.Errorf("definition result %T, want []lsp.Location", value)
	}
	return locations, nil
}

func TestProductionDefinitionRejectsSameDocumentOldBinding(t *testing.T) {
	s := newDefinitionServer(t)
	oldProvider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END /* database A */")
	oldLease := readyHoverLease(t, s, "definition-old-binding", oldProvider)
	newProvider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END /* database B */")
	newLease := readyHoverLease(t, s, "definition-new-binding", newProvider)
	const text = "execute procedure myproc"
	s.files[testFileURI] = &File{Text: text, Version: 7, Revision: 11}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, oldLease
	type answer struct {
		locations []lsp.Location
		err       error
	}
	oldAnswer := make(chan answer, 1)
	go func() { locations, err := invokeDefinition(s, text, 20); oldAnswer <- answer{locations, err} }()
	waitSignal(t, oldProvider.entered, "old definition Detail.Load")
	s.stateMu.Lock()
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 2, 2, newLease
	s.stateMu.Unlock()
	close(oldProvider.release)
	old := <-oldAnswer
	if old.err != nil || len(old.locations) != 0 {
		t.Fatalf("old binding result = (%#v, %v), want no location", old.locations, old.err)
	}
	if !snapshotRootIsEmpty(t, s.snapshots) {
		t.Fatal("stale A definition left a source candidate on disk")
	}
	newResponse := make(chan answer, 1)
	go func() { locations, err := invokeDefinition(s, text, 20); newResponse <- answer{locations, err} }()
	waitSignal(t, newProvider.entered, "new definition Detail.Load")
	close(newProvider.release)
	newResult := <-newResponse
	newLocations, err := newResult.locations, newResult.err
	if err != nil || len(newLocations) != 1 {
		t.Fatalf("new binding result = (%#v, %v), want one location", newLocations, err)
	}
	content, err := os.ReadFile(snapshotPathFromURI(t, newLocations[0].URI))
	if err != nil || !strings.Contains(string(content), "database B") {
		t.Fatalf("new source = %q, %v, want database B", content, err)
	}
}

func TestProductionDefinitionRechecksSaveAndDialectAfterDetail(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*Server)
	}{{"didSave", func(s *Server) { s.files[testFileURI].Revision++ }}, {"dialect", func(s *Server) {
		s.dbConn = &database.DBConnection{Driver: dialect.DatabaseDriverInterBase, Variant: dialect.SQLVariantInterBase1}
	}}} {
		t.Run(mutate.name, func(t *testing.T) {
			s := newDefinitionServer(t)
			provider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END")
			lease := readyHoverLease(t, s, "definition-stale-"+mutate.name, provider)
			const text = "execute procedure myproc"
			s.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
			s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, lease
			answer := make(chan struct {
				locations []lsp.Location
				err       error
			}, 1)
			go func() {
				locations, err := invokeDefinition(s, text, 20)
				answer <- struct {
					locations []lsp.Location
					err       error
				}{locations, err}
			}()
			waitSignal(t, provider.entered, "definition Detail.Load")
			s.stateMu.Lock()
			mutate.apply(s)
			s.stateMu.Unlock()
			close(provider.release)
			got := <-answer
			if got.err != nil || len(got.locations) != 0 {
				t.Fatalf("stale result = (%#v, %v), want no location", got.locations, got.err)
			}
		})
	}
}

func TestProductionDefinitionRemovesOnlyStaleCandidate(t *testing.T) {
	s := newDefinitionServer(t)
	oldProvider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END /* A */")
	oldLease := readyHoverLease(t, s, "definition-candidate-A", oldProvider)
	newProvider := newGatedHoverProvider("MYPROC", "CREATE PROCEDURE MYPROC AS BEGIN SUSPEND; END /* B */")
	newLease := readyHoverLease(t, s, "definition-candidate-B", newProvider)
	const text = "execute procedure myproc"
	s.files[testFileURI] = &File{Text: text, Version: 2, Revision: 2}
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, oldLease
	writeEntered, writeRelease := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	s.snapshots.afterCandidateWrite = func() {
		if first.CompareAndSwap(false, true) {
			close(writeEntered)
			<-writeRelease
		}
	}
	type answer struct {
		locations []lsp.Location
		err       error
	}
	oldAnswer := make(chan answer, 1)
	go func() { locations, err := invokeDefinition(s, text, 20); oldAnswer <- answer{locations, err} }()
	waitSignal(t, oldProvider.entered, "candidate A detail")
	close(oldProvider.release)
	waitSignal(t, writeEntered, "candidate A file write")
	s.stateMu.Lock()
	s.connGeneration, s.metaBoundGeneration, s.metaLease = 2, 2, newLease
	s.stateMu.Unlock()
	newAnswer := make(chan answer, 1)
	go func() { locations, err := invokeDefinition(s, text, 20); newAnswer <- answer{locations, err} }()
	waitSignal(t, newProvider.entered, "candidate B detail")
	close(newProvider.release)
	new := <-newAnswer
	if new.err != nil || len(new.locations) != 1 {
		t.Fatalf("B result=(%#v,%v), want one location", new.locations, new.err)
	}
	close(writeRelease)
	old := <-oldAnswer
	if old.err != nil || len(old.locations) != 0 {
		t.Fatalf("A result=(%#v,%v), want no location", old.locations, old.err)
	}
	content, err := os.ReadFile(snapshotPathFromURI(t, new.locations[0].URI))
	if err != nil || !strings.Contains(string(content), "/* B */") {
		t.Fatalf("B snapshot=%q, %v", content, err)
	}
	if !strings.Contains(new.locations[0].URI, ".candidate-") {
		t.Fatalf("URI %q does not identify private candidate", new.locations[0].URI)
	}
}

func TestProductionDefinitionPreservesLocalJumpsAndOffsetsNativeSource(t *testing.T) {
	t.Run("local alias", func(t *testing.T) {
		s := newDefinitionServer(t)
		text := "SELECT a.id FROM things a"
		s.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
		s.connGeneration = 1
		locations, err := invokeDefinition(s, text, strings.Index(text, "a.id"))
		if err != nil || len(locations) != 1 || locations[0].URI != testFileURI {
			t.Fatalf("alias result=(%#v,%v), want local document jump", locations, err)
		}
		if !snapshotRootIsEmpty(t, s.snapshots) {
			t.Fatal("local alias navigation wrote a source file")
		}
	})
	t.Run("procedure local", func(t *testing.T) {
		s := newDefinitionServer(t)
		text := "ALTER PROCEDURE p AS\nDECLARE VARIABLE value INTEGER;\nBEGIN\nvalue = :value;\nEND"
		s.files[testFileURI] = &File{Text: text, Version: 1, Revision: 1}
		s.connGeneration = 1
		params, _ := json.Marshal(lsp.DefinitionParams{TextDocumentPositionParams: lsp.TextDocumentPositionParams{TextDocument: lsp.TextDocumentIdentifier{URI: testFileURI}, Position: lsp.Position{Line: 3, Character: 0}}})
		raw := json.RawMessage(params)
		value, err := s.handleDefinition(context.Background(), nil, &jsonrpc2.Request{Params: &raw})
		locations, _ := value.([]lsp.Location)
		if err != nil || len(locations) != 1 || locations[0].URI != testFileURI {
			t.Fatalf("local result=(%#v,%v), want local document jump", locations, err)
		}
		if !snapshotRootIsEmpty(t, s.snapshots) {
			t.Fatal("procedure local navigation wrote a source file")
		}
	})
	t.Run("headerless native source range", func(t *testing.T) {
		s := newDefinitionServer(t)
		provider := newGatedHoverProvider("MYPROC", "")
		provider.native = "BEGIN\n  SUSPEND;\nEND"
		lease := readyHoverLease(t, s, "definition-headerless-native", provider)
		const text = "execute procedure myproc"
		s.files[testFileURI] = &File{Text: text, Version: 4, Revision: 9}
		s.connGeneration, s.metaBoundGeneration, s.metaLease = 1, 1, lease
		response := make(chan struct {
			locations []lsp.Location
			err       error
		}, 1)
		go func() {
			locations, err := invokeDefinition(s, text, 20)
			response <- struct {
				locations []lsp.Location
				err       error
			}{locations, err}
		}()
		waitSignal(t, provider.entered, "unsupported DDL detail")
		close(provider.release)
		got := <-response
		if got.err != nil || len(got.locations) != 1 {
			t.Fatalf("native source result=(%#v,%v), want one", got.locations, got.err)
		}
		if got.locations[0].Range.Start != (lsp.Position{Line: 6, Character: 0}) || got.locations[0].Range.End != (lsp.Position{Line: 6, Character: 0}) {
			t.Fatalf("headerless source range=%+v, want explicit body-start offset after six banner lines", got.locations[0].Range)
		}
		content, err := os.ReadFile(snapshotPathFromURI(t, got.locations[0].URI))
		if err != nil || !strings.Contains(string(content), "provenance: native-source") || !strings.Contains(string(content), provider.native) {
			t.Fatalf("native snapshot=%q, %v", content, err)
		}
	})
}

func snapshotPathFromURI(t *testing.T, value string) string {
	t.Helper()
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "file" {
		t.Fatalf("snapshot URI %q: %v", value, err)
	}
	return u.Path
}

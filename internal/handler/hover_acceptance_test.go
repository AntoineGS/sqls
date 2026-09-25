package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sourcegraph/jsonrpc2"
	"github.com/sqls-server/sqls/catalog"
	"github.com/sqls-server/sqls/internal/database"
	"github.com/sqls-server/sqls/internal/lsp"
	"github.com/sqls-server/sqls/language"
	"github.com/sqls-server/sqls/metadata"
	"github.com/sqls-server/sqls/metadata/metadatatest"
)

type gatedHoverProvider struct {
	name    string
	ddl     string
	entered chan struct{}
	release chan struct{}
	done    chan struct{}
	loads   atomic.Int64
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
	if req.Object.Kind != catalog.ObjectKindProcedure || req.Object.Name != p.name || req.Facet != "ddl" {
		return metadata.DetailPlan{}, metadata.ErrUnsupported
	}
	return metadata.DetailPlan{Requires: []catalog.MetadataKind{database.MetadataProcedures}, Coverage: req.Object.Key() + "\x00" + req.Facet,
		Load: func(context.Context, *catalog.Cache) (metadata.Detail, error) {
			p.loads.Add(1)
			p.entered <- struct{}{}
			<-p.release // intentionally ignores the detached load context
			close(p.done)
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

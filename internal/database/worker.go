package database

import (
	"context"
	"log"
	"sync"
)

type Worker struct {
	dbRepo         DBRepository
	dbCache        *DBCache
	onCacheChanged func()

	done     chan struct{}
	update   chan struct{}
	lock     sync.Mutex
	stopOnce sync.Once
}

func NewWorker() *Worker {
	return &Worker{
		done:   make(chan struct{}, 1),
		update: make(chan struct{}, 1),
	}
}

func (w *Worker) Cache() *DBCache {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.dbCache
}

func (w *Worker) setCache(c *DBCache) {
	w.lock.Lock()
	w.dbCache = c
	callback := w.onCacheChanged
	w.lock.Unlock()
	if callback != nil {
		callback()
	}
}

// SetCacheChangedCallback registers a callback invoked after any cache
// replacement. The callback runs without the worker lock held.
func (w *Worker) SetCacheChangedCallback(callback func()) {
	w.lock.Lock()
	w.onCacheChanged = callback
	hasCache := w.dbCache != nil
	w.lock.Unlock()
	if callback != nil && hasCache {
		callback()
	}
}

// repo returns the repository the worker goroutine should use. ReCache runs on
// the handler goroutine and may replace it while the worker goroutine is
// servicing an update, so both sides go through w.lock.
func (w *Worker) repo() DBRepository {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.dbRepo
}

func (w *Worker) setRepo(repo DBRepository) {
	w.lock.Lock()
	defer w.lock.Unlock()
	w.dbRepo = repo
}

func (w *Worker) setColumnCache(col map[string][]*ColumnDesc) {
	w.lock.Lock()
	changed := false
	if w.dbCache != nil {
		// Swap in a copy so that readers holding the previous
		// *DBCache keep seeing a consistent snapshot.
		newCache := *w.dbCache
		newCache.ColumnsWithParent = col
		w.dbCache = &newCache
		changed = true
	}
	callback := w.onCacheChanged
	w.lock.Unlock()
	if changed && callback != nil {
		callback()
	}
}

func (w *Worker) setCatalogCache(c *CatalogCache) {
	w.lock.Lock()
	changed := false
	if w.dbCache != nil {
		// Swap in a copy so that readers holding the previous
		// *DBCache keep seeing a consistent snapshot.
		newCache := *w.dbCache
		newCache.Catalog = c
		w.dbCache = &newCache
		changed = true
	}
	callback := w.onCacheChanged
	w.lock.Unlock()
	if changed && callback != nil {
		callback()
	}
}

func (w *Worker) Start() {
	go func() {
		log.Println("db worker: start")
		for {
			select {
			case <-w.done:
				log.Println("db worker: done")
				return
			case <-w.update:
				generator := NewDBCacheUpdater(w.repo())
				// The two passes are independent. This loop used to continue
				// on a secondary-pass error, so appending the catalog build
				// after it would silently skip the catalog whenever the column
				// pass failed.
				if col, err := generator.GenerateDBCacheSecondary(context.Background()); err != nil {
					log.Println(err)
				} else {
					w.setColumnCache(col)
					log.Println("db worker: Update db cache secondary complete")
				}
				if catalog, ok, err := generator.GenerateCatalogCache(context.Background()); err != nil {
					// A catalog error leaves the previous *CatalogCache in
					// place, exactly as the column pass does.
					log.Println(err)
				} else if ok {
					w.setCatalogCache(catalog)
					log.Println("db worker: Update catalog cache complete")
				}
			}
		}
	}()
}

// Stop is safe to call more than once: handleExit and the deferred Stop in
// main both reach it on an ordinary shutdown.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
}

// Done reports the worker's shutdown channel. It exists so callers can assert
// that Stop has run.
func (w *Worker) Done() <-chan struct{} {
	return w.done
}

func (w *Worker) ReCache(ctx context.Context, repo DBRepository) error {
	w.setRepo(repo)
	if err := w.updateAllCache(ctx); err != nil {
		return err
	}
	w.updateAdditionalCache()
	return nil
}

func (w *Worker) updateAllCache(ctx context.Context) error {
	generator := NewDBCacheUpdater(w.repo())
	cache, err := generator.GenerateDBCachePrimary(ctx)
	if err != nil {
		return err
	}
	w.setCache(cache)
	log.Println("db worker: Update db cache primary complete")
	return nil
}

func (w *Worker) updateAdditionalCache() {
	// Non-blocking: this is reached from an LSP handler through ReCache, and a
	// long catalog pass must not make a configuration change wait. A full slot
	// already holds a pending request, so dropping a duplicate signal loses
	// nothing — the in-flight or queued pass will read state that is at least
	// as fresh.
	select {
	case w.update <- struct{}{}:
	default:
	}
}

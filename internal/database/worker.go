package database

import (
	"context"
	"log"
	"sync"
)

type Worker struct {
	dbRepo  DBRepository
	dbCache *DBCache

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
	defer w.lock.Unlock()
	w.dbCache = c
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
	defer w.lock.Unlock()
	if w.dbCache != nil {
		// Swap in a copy so that readers holding the previous
		// *DBCache keep seeing a consistent snapshot.
		newCache := *w.dbCache
		newCache.ColumnsWithParent = col
		w.dbCache = &newCache
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
				col, err := generator.GenerateDBCacheSecondary(context.Background())
				if err != nil {
					log.Println(err)
					continue
				}
				w.setColumnCache(col)
				log.Println("db worker: Update db cache secondary complete")
			}
		}
	}()
}

// Stop is safe to call more than once: handleExit and the deferred Stop in
// main both reach it on an ordinary shutdown.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.done) })
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
	w.update <- struct{}{}
}

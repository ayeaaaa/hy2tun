package manager

import (
	"context"
	"sync"
)

// RefreshAll refreshes every configured server with bounded concurrency. Each
// worker holds one semaphore slot for a single Refresh, and Refresh itself
// serializes per server, so one server receives at most one in-flight request
// from this process. The composition root owns the background ticker; this
// method does not start one.
func (i *Inventory) RefreshAll(ctx context.Context) {
	if i == nil || i.store == nil {
		return
	}
	records := i.store.State().Servers
	if len(records) == 0 {
		return
	}

	semaphore := make(chan struct{}, inventoryRefreshConcurrency)
	var wait sync.WaitGroup
	for _, record := range records {
		id := record.ID
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			if _, err := i.Refresh(ctx, id); err != nil {
				i.log.Debug("background server refresh failed", "server", id, "error", err)
			}
		}()
	}
	wait.Wait()
}

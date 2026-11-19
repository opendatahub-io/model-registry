package cmd

import "sync"

// backendCleanupOwner owns cleanup even when startup finishes after shutdown.
type backendCleanupOwner struct {
	mu      sync.Mutex
	closed  bool
	cleanup func() error
}

func (o *backendCleanupOwner) Install(cleanup func() error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		if cleanup != nil {
			_ = cleanup()
		}
		return
	}
	o.cleanup = cleanup
}

func (o *backendCleanupOwner) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	if o.cleanup != nil {
		return o.cleanup()
	}
	return nil
}

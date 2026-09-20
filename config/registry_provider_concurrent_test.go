package config

import (
	"sync"
	"testing"
	"time"
)

// TestRegistry_GetProvider_ConcurrentWithReload is a regression test for audit
// H1: the Providers map is mutated in place by the config file-watcher reload
// (loader.go), so any concurrent unlocked r.Providers[...] read fatal-panics
// with "concurrent map read and map write". GetProvider / DefaultProviderName
// are the locked accessors all hot-path reads MUST use. This test hammers them
// while a writer mutates Providers under Lock — before the accessors existed
// (direct map reads in spawner/dag_walker/reflection/role_generator) this
// crashed at runtime.
func TestRegistry_GetProvider_ConcurrentWithReload(t *testing.T) {
	r := &Registry{
		Providers:       make(map[string]*ProviderConfig),
		DefaultProvider: "main",
	}
	r.Providers["main"] = &ProviderConfig{Model: "m1"}
	r.Providers["weak"] = &ProviderConfig{Model: "m2"}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: simulates config reload mutating Providers in place under Lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.Mu.Lock()
			r.Providers["weak"] = &ProviderConfig{Model: "m2"}
			r.Mu.Unlock()
		}
	}()

	// Readers: the locked accessors. Direct r.Providers[...] here would race.
	const readers = 8
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				_ = r.GetProvider(r.DefaultProviderName())
				_ = r.GetProvider("weak")
			}
		}()
	}

	// Let them overlap, then stop the writer.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Sanity: accessor still returns the configured provider.
	if pc := r.GetProvider(r.DefaultProviderName()); pc == nil || pc.Model != "m1" {
		t.Fatalf("expected default provider m1, got %v", pc)
	}
}

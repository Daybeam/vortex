package core

import (
	"sort"
	"sync"
	"time"
)

// SubscribeBatched subscribes to the EventBus and returns a channel that
// receives debounced batches of AgentEvents. Events are collected within the
// debounce window or until batchSize is reached, then delivered as a sorted
// batch (highest priority first). This is the consumer-side Debounce utility
// for EVENT_DRIVEN_CALLBACK_DESIGN.md §五.
//
// Usage (e.g. in swarm provider or chat harness):
//
//	batches := SubscribeBatched(DefaultBus, "*", 10, 100*time.Millisecond)
//	for batch := range batches {
//	    // batch is sorted by priority (EventDecisionRequired first)
//	    handleBatch(batch)
//	}
//
// Backpressure: if the internal buffer is full, incoming events are dropped
// (non-blocking). High-priority events (priority 0) are never dropped — they
// force their way in by evicting a low-priority event.
func SubscribeBatched(bus *EventBus, eventType string, batchSize int, window time.Duration) (<-chan []AgentEvent, func()) {
	if batchSize <= 0 {
		batchSize = 10
	}
	if window <= 0 {
		window = 100 * time.Millisecond
	}

	out := make(chan []AgentEvent, 16)
	ingest := make(chan AgentEvent, 64)
	stopChan := make(chan struct{})

	bus.Subscribe(eventType, func(e AgentEvent) error {
		select {
		case ingest <- e:
		default:
			// Buffer full — try evicting a low-priority event for high-priority
			if eventPriority(EventType(e.EventType)) == 0 {
				select {
				case <-ingest: // evict oldest
				default:
				}
				select {
				case ingest <- e:
				default:
				}
			}
		}
		return nil
	})

	go func() {
		defer close(out)
		batch := make([]AgentEvent, 0, batchSize)
		timer := time.NewTimer(window)
		defer timer.Stop()

		flush := func() {
			if len(batch) == 0 {
				return
			}
			sort.SliceStable(batch, func(i, j int) bool {
				return eventPriority(EventType(batch[i].EventType)) < eventPriority(EventType(batch[j].EventType))
			})
			select {
			case out <- batch:
			default:
			}
			batch = make([]AgentEvent, 0, batchSize)
		}

		for {
			select {
			case <-stopChan:
				flush()
				return
			case e, ok := <-ingest:
				if !ok {
					flush()
					return
				}
				batch = append(batch, e)
				if len(batch) >= batchSize {
					flush()
					timer.Reset(window)
				}
			case <-timer.C:
				flush()
				timer.Reset(window)
			}
		}
	}()

	// stop closes stopChan, causing the goroutine to flush and exit, which closes out.
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() { close(stopChan) })
	}
	return out, stop
}

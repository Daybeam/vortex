//go:build test
// +build test

package core

import (
	"testing"
	"time"
)

func TestDirectedEngine_Broadcast(t *testing.T) {
	engine := &DirectedEngine{
		notifyChan: make(chan struct{}),
	}

	ch1 := engine.GetNotifyChan()
	done := make(chan bool)
	go func() {
		select {
		case <-ch1:
			done <- true
		case <-time.After(100 * time.Millisecond):
			done <- false
		}
	}()

	engine.Broadcast()
	if !<-done {
		t.Error("Broadcast did not trigger notify channel")
	}
}

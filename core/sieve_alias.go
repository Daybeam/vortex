package core

import (
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/sieve"
)

const toolNamespaceSeparator = sieve.ToolNamespaceSeparator

// Transitional bridge for call sites not yet migrated to core/sieve.
type Sieve = sieve.Sieve

func NewSieve(maxKeep int) *Sieve {
	return sieve.NewSieve(maxKeep)
}

func newSieveFromConfig(sys config.SystemSettings) *Sieve {
	return sieve.NewSieveFromConfig(sys)
}

func isReadTool(name string) bool {
	return sieve.IsReadTool(name)
}

func isPollerTool(name string) bool {
	return sieve.IsPollerTool(name)
}

// isPollingCall is the call-level form: a tool can be a poller without saying
// so in its name, as long as the call asks to block or queries a task status.
func isPollingCall(name string, args map[string]any) bool {
	return sieve.IsPollingCall(name, args)
}

package core

import (
	"github.com/daybeam/vortex/pkg/observability"
)

// Alias functions for backward compatibility within core package.
var (
	NewTraceID       = observability.NewTraceID
	NewSpanID        = observability.NewSpanID
	ContextWithTrace = observability.ContextWithTrace
	GetTraceID       = observability.GetTraceID
	GetSpanID        = observability.GetSpanID
	StartSpan        = observability.StartSpan
	Redact           = observability.Redact
	RedactMap        = observability.RedactMap
)

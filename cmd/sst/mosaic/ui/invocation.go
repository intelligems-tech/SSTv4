package ui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/sst/sst/v3/cmd/sst/mosaic/aws"
)

// The handler a pooled request runs, as its bundle's describe() reported.
type invocationHandler struct {
	key   string
	label string
}

// invocationName names an invocation: the handler label a pooled bundle
// reported, else the function's handler.
func (u *UI) invocationName(evt *aws.FunctionInvokedEvent) string {
	if evt.Label == "" {
		return u.functionName(evt.FunctionID)
	}
	if evt.Handler != "" && evt.Handler != evt.Label {
		return evt.Label + "  " + TEXT_DIM.Render(evt.Handler)
	}
	return evt.Label
}

// matchInvocation applies the filter to a function, or to the handler a
// pooled request runs.
func (u *UI) matchInvocation(functionID string, requestID string) bool {
	if u.matchFilter(functionID) {
		return true
	}
	handler, ok := u.handlers[requestID]
	if !ok {
		return false
	}
	filter := strings.ToLower(u.options.Filter)
	return strings.Contains(strings.ToLower(handler.key), filter) ||
		strings.Contains(strings.ToLower(handler.label), filter)
}

// DurationStyle colours an invocation duration: under a second is fine,
// under three is slow, anything longer is a problem.
func DurationStyle(duration time.Duration) lipgloss.Style {
	switch {
	case duration < time.Second:
		return TEXT_SUCCESS
	case duration < 3*time.Second:
		return TEXT_WARNING
	default:
		return TEXT_DANGER
	}
}

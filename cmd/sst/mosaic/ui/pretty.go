package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/sst/sst/v3/pkg/prettyjson"
)

// printFunctionLog prints a function log line. In pretty JSON mode a line
// carrying JSON prints coloured over several rows, indented under the
// message column.
func (u *UI) printFunctionLog(barColor lipgloss.Style, label string, line string) {
	line = u.formatFunctionLogLine(line)
	if prettyjson.Enabled() {
		if lines, ok := prettyjson.FormatLogLine(line); ok {
			rows := strings.Split(prettyjson.ANSI(lines), "\n")
			u.printEvent(barColor, label, rows[0])
			pad := strings.Repeat(" ", 12)
			for _, row := range rows[1:] {
				u.printEvent(barColor, "", pad+row)
			}
			return
		}
	}
	u.printEvent(barColor, label, line)
}

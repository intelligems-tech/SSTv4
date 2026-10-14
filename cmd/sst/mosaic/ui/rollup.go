package ui

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
)

// Deploy output roll-up. Routine updates fold into one summary line; created,
// deleted and imported resources print one compact line each, named by the
// SST component they belong to, so a deploy of fifty functions reads as a
// handful of lines instead of fifty wrapped ones.

type rollupEntry struct {
	name     string
	icon     string
	duration time.Duration
}

type rollup struct {
	updated map[string]*rollupEntry
	printed map[string]bool
}

func newRollup() *rollup {
	return &rollup{updated: map[string]*rollupEntry{}, printed: map[string]bool{}}
}

// rollupActive reports whether the current command rolls its output up.
func (u *UI) rollupActive() bool {
	return u.mode == ProgressModeDeploy || u.mode == ProgressModeRemove
}

// rollupOutputs handles a finished resource. It returns false when the
// upstream per-resource line should print instead.
func (u *UI) rollupOutputs(evt *apitype.ResOutputsEvent, duration time.Duration) bool {
	if !u.rollupActive() {
		return false
	}
	if u.rollup == nil {
		u.rollup = newRollup()
	}
	urn, name, icon := u.rollupRoot(evt.Metadata.URN)
	switch evt.Metadata.Op {
	case apitype.OpUpdate:
		entry, ok := u.rollup.updated[urn]
		if !ok {
			entry = &rollupEntry{name: name, icon: icon}
			u.rollup.updated[urn] = entry
		}
		entry.duration = max(entry.duration, duration)
	case apitype.OpCreate, apitype.OpCreateReplacement:
		u.rollupLine(urn, "+", TEXT_SUCCESS, icon, name, "created", duration)
	case apitype.OpImport:
		u.rollupLine(urn, "+", TEXT_SUCCESS, icon, name, "imported", duration)
	case apitype.OpDelete, apitype.OpDeleteReplaced:
		u.rollupLine(urn, "-", TEXT_DANGER, icon, name, "deleted", duration)
	}
	return true
}

// rollupLine prints one line per component and operation: a new function
// creates a role, a log group and the function, and reads as one creation.
func (u *UI) rollupLine(urn string, sign string, style lipgloss.Style, icon string, name string, verb string, duration time.Duration) {
	key := urn + "|" + verb
	if u.rollup.printed[key] {
		return
	}
	u.rollup.printed[key] = true
	u.println(
		"  ",
		style.Render(sign),
		" ",
		TEXT_DIM.Render(icon),
		" ",
		TEXT_NORMAL.Render(fmt.Sprintf("%-40s", name)),
		" ",
		TEXT_DIM.Render(fmt.Sprintf("%-8s %s", verb, formatRollupDuration(duration))),
	)
}

// printRollupSummary prints the folded updates: a count by kind and the
// slowest few.
func (u *UI) printRollupSummary() {
	if u.rollup == nil || len(u.rollup.updated) == 0 {
		return
	}
	kinds := map[string]int{}
	entries := make([]*rollupEntry, 0, len(u.rollup.updated))
	for _, entry := range u.rollup.updated {
		kinds[entry.icon]++
		entries = append(entries, entry)
	}
	icons := make([]string, 0, len(kinds))
	for icon := range kinds {
		icons = append(icons, icon)
	}
	sort.Slice(icons, func(i, j int) bool {
		if kinds[icons[i]] != kinds[icons[j]] {
			return kinds[icons[i]] > kinds[icons[j]]
		}
		return icons[i] < icons[j]
	})
	counts := []string{}
	for _, icon := range icons {
		counts = append(counts, fmt.Sprintf("%s %d", icon, kinds[icon]))
	}
	u.blank()
	u.println(
		"  ",
		TEXT_WARNING.Render("~"),
		" ",
		TEXT_NORMAL_BOLD.Render(fmt.Sprintf("%d updated", len(entries))),
		"   ",
		TEXT_DIM.Render(strings.Join(counts, "  ")),
	)
	sort.Slice(entries, func(i, j int) bool { return entries[i].duration > entries[j].duration })
	slowest := []string{}
	for _, entry := range entries[:min(3, len(entries))] {
		slowest = append(slowest, entry.name+" "+formatRollupDuration(entry.duration))
	}
	u.println("    ", TEXT_DIM.Render("slowest  "+strings.Join(slowest, " · ")))
	u.rollup.updated = map[string]*rollupEntry{}
}

// rollupRoot names a resource by its nearest SST component, itself included,
// or else its outermost ancestor.
func (u *UI) rollupRoot(urn string) (string, string, string) {
	current := resource.URN(urn)
	outermost := current
	for current != "" {
		if strings.HasPrefix(string(current.Type()), "sst:") && !slices.Contains(IGNORED_RESOURCES, string(current.Type())) {
			return string(current), rollupName(current), rollupIcon(string(current.Type()))
		}
		outermost = current
		parent := resource.URN(u.parents[string(current)])
		if parent == "" || slices.Contains(IGNORED_RESOURCES, string(parent.Type())) {
			break
		}
		current = parent
	}
	return string(outermost), rollupName(outermost), rollupIcon(string(outermost.Type()))
}

func rollupName(urn resource.URN) string {
	name, _, _ := strings.Cut(urn.Name(), ".")
	return name
}

// rollupIcon is a one-glyph kind for a resource type.
func rollupIcon(typ string) string {
	lower := strings.ToLower(typ)
	switch {
	case strings.Contains(lower, "function") || strings.HasPrefix(lower, "aws:lambda"):
		return "λ"
	case strings.Contains(lower, "bucket") || strings.HasPrefix(lower, "aws:s3"):
		return "▣"
	case strings.Contains(lower, "queue") || strings.HasPrefix(lower, "aws:sqs"):
		return "≡"
	case strings.Contains(lower, "cron") || strings.HasPrefix(lower, "aws:cloudwatch:eventrule") || strings.HasPrefix(lower, "aws:scheduler"):
		return "◷"
	case strings.Contains(lower, "apigateway") || strings.Contains(lower, "router"):
		return "⇄"
	case strings.Contains(lower, "role") || strings.Contains(lower, "policy") || strings.HasPrefix(lower, "aws:iam"):
		return "⚿"
	default:
		return "•"
	}
}

func formatRollupDuration(duration time.Duration) string {
	if duration < time.Second {
		return fmt.Sprintf("%dms", duration.Milliseconds())
	}
	if duration < time.Minute {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	return duration.Round(time.Second).String()
}

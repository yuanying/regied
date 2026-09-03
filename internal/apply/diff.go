package apply

import (
	"fmt"
	"strings"
)

// contextLines is how much of what did not change is shown around what did. Three is
// what every diff an operator has read shows, and the point of a dry-run is to be read
// without translation.
const contextLines = 3

// unifiedDiff is the difference between two texts, in the format diff -u produces, or
// the empty string when they are the same.
//
// It is written here rather than taken from somewhere because the whole of the engine
// has one dependency and the diff is a hundred lines. It also means an apply on a host
// with no diff installed still shows one (ADR 0006).
func unifiedDiff(before, after string) string {
	if before == after {
		return ""
	}
	old, new := splitLines(before), splitLines(after)
	edits := lineEdits(old, new)

	var b strings.Builder
	for _, hunk := range hunksOf(edits) {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n",
			hunk.oldStart, hunk.oldCount, hunk.newStart, hunk.newCount)
		for _, edit := range edits[hunk.from:hunk.to] {
			b.WriteString(string(edit.op) + edit.text + "\n")
		}
	}
	return b.String()
}

// splitLines is the lines of a text, without a trailing empty one for the final
// newline. A file with no trailing newline and one with it differ by nothing an operator
// wants to read about.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// edit is one line of the diff: kept, removed or added.
type edit struct {
	op   byte // ' ', '-', '+'
	text string
}

// lineEdits turns two lists of lines into the edits that take one to the other, keeping
// as much as possible. The table is the ordinary longest-common-subsequence one; the
// texts here are configuration files, so its size is not a concern.
func lineEdits(old, new []string) []edit {
	lengths := make([][]int, len(old)+1)
	for i := range lengths {
		lengths[i] = make([]int, len(new)+1)
	}
	for i := len(old) - 1; i >= 0; i-- {
		for j := len(new) - 1; j >= 0; j-- {
			if old[i] == new[j] {
				lengths[i][j] = lengths[i+1][j+1] + 1
			} else {
				lengths[i][j] = max(lengths[i+1][j], lengths[i][j+1])
			}
		}
	}

	var edits []edit
	i, j := 0, 0
	for i < len(old) && j < len(new) {
		switch {
		case old[i] == new[j]:
			edits = append(edits, edit{' ', old[i]})
			i, j = i+1, j+1
		case lengths[i+1][j] >= lengths[i][j+1]:
			edits = append(edits, edit{'-', old[i]})
			i++
		default:
			edits = append(edits, edit{'+', new[j]})
			j++
		}
	}
	for ; i < len(old); i++ {
		edits = append(edits, edit{'-', old[i]})
	}
	for ; j < len(new); j++ {
		edits = append(edits, edit{'+', new[j]})
	}
	return edits
}

// hunk is a run of edits worth printing together, and where it sits in each text.
type hunk struct {
	from, to           int
	oldStart, oldCount int
	newStart, newCount int
}

// hunksOf groups the edits into the runs a unified diff prints, each holding the changes
// and up to contextLines of what did not change on either side.
func hunksOf(edits []edit) []hunk {
	var out []hunk
	oldLine, newLine := 1, 1
	// starts holds, for each edit, the line numbers it begins at.
	oldAt := make([]int, len(edits))
	newAt := make([]int, len(edits))
	for i, e := range edits {
		oldAt[i], newAt[i] = oldLine, newLine
		if e.op != '+' {
			oldLine++
		}
		if e.op != '-' {
			newLine++
		}
	}

	for i := 0; i < len(edits); {
		if edits[i].op == ' ' {
			i++
			continue
		}
		from := max(0, i-contextLines)
		to := i
		for to < len(edits) {
			if edits[to].op != ' ' {
				to++
				continue
			}
			// Keep going while another change is close enough that the context would
			// touch: two hunks that overlap read worse than one.
			next := to
			for next < len(edits) && edits[next].op == ' ' {
				next++
			}
			if next < len(edits) && next-to <= 2*contextLines {
				to = next
				continue
			}
			to = min(len(edits), to+contextLines)
			break
		}

		out = append(out, hunk{
			from: from, to: to,
			oldStart: oldAt[from], oldCount: countOp(edits[from:to], '+'),
			newStart: newAt[from], newCount: countOp(edits[from:to], '-'),
		})
		i = to
	}
	return out
}

// countOp is how many of a hunk's lines belong to one side: every line except the ones
// only the other side has.
func countOp(edits []edit, exclude byte) int {
	var n int
	for _, e := range edits {
		if e.op != exclude {
			n++
		}
	}
	return n
}

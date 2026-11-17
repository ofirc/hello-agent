package agentloop

import (
	"fmt"
	"strings"
)

// diffContext is the number of unchanged lines shown around each change, as diff -u.
const diffContext = 3

type diffOp struct {
	kind byte // ' ', '-' or '+'
	line string
}

// UnifiedDiff is a line diff of oldText and newText in unified format, for showing a
// proposal to the human who confirms it. Equal inputs give "".
func UnifiedDiff(path, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	ops := diffLines(splitLines(oldText), splitLines(newText))

	// oldAt and newAt are how many lines of each side come before ops[k].
	oldAt := make([]int, len(ops)+1)
	newAt := make([]int, len(ops)+1)
	for k, op := range ops {
		oldAt[k+1], newAt[k+1] = oldAt[k], newAt[k]
		if op.kind != '+' {
			oldAt[k+1]++
		}
		if op.kind != '-' {
			newAt[k+1]++
		}
	}

	var b strings.Builder
	b.WriteString("--- a/" + path + "\n+++ b/" + path + "\n")
	for i := 0; i < len(ops); {
		for i < len(ops) && ops[i].kind == ' ' {
			i++
		}
		if i == len(ops) {
			break
		}
		// A hunk runs until the unchanged gap is too long to be shared context for the
		// next change, as diff -u merges hunks closer than twice the context.
		end := i + 1
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j + 1
			} else if j-end >= 2*diffContext {
				break
			}
		}
		from, to := max(i-diffContext, 0), min(end+diffContext, len(ops))
		fmt.Fprintf(&b, "@@ -%s +%s @@\n",
			hunkRange(oldAt[from], oldAt[to]-oldAt[from]), hunkRange(newAt[from], newAt[to]-newAt[from]))
		for _, op := range ops[from:to] {
			b.WriteByte(op.kind)
			b.WriteString(op.line)
			if !strings.HasSuffix(op.line, "\n") {
				b.WriteString("\n\\ No newline at end of file\n")
			}
		}
		i = end
	}
	return b.String()
}

// hunkRange is "start,count" with a 1-based start. An empty range starts at the line
// before it, so inserting at the top of a file reads -0,0.
func hunkRange(before, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", before)
	}
	return fmt.Sprintf("%d,%d", before+1, count)
}

// splitLines keeps each line's newline, so a last line without one stays distinct from
// the same line with one.
func splitLines(s string) []string {
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines is the edit script from a to b by longest common subsequence. The common
// prefix and suffix are taken off first: a proposal usually changes one line, so the
// table left is tiny, and the O(n·m) table is only paid for the part that changed.
func diffLines(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}

	x, y := a[pre:len(a)-suf], b[pre:len(b)-suf]
	n, m := len(x), len(y)
	// lcs[i*(m+1)+j] is the LCS length of x[i:] and y[j:].
	lcs := make([]int32, (n+1)*(m+1))
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i*(m+1)+j] = lcs[(i+1)*(m+1)+j+1] + 1
			} else {
				lcs[i*(m+1)+j] = max(lcs[(i+1)*(m+1)+j], lcs[i*(m+1)+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			ops = append(ops, diffOp{' ', x[i]})
			i++
			j++
		case j == m || (i < n && lcs[(i+1)*(m+1)+j] >= lcs[i*(m+1)+j+1]):
			ops = append(ops, diffOp{'-', x[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', y[j]})
			j++
		}
	}

	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

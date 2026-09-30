package ui

import (
	"fmt"
	"strings"
)

// DiffContext is how many unchanged lines surround each hunk.
const DiffContext = 3

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

// UnifiedDiff returns a unified diff of a → b, or "" when they are equal.
func UnifiedDiff(aName, bName, a, b string) string {
	if a == b {
		return ""
	}
	al, bl := splitLines(a), splitLines(b)
	ops := diffLines(al, bl)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	// Walk the ops, cutting hunks around changes with DiffContext lines.
	ai, bi := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, op := range ops {
		ai[i+1], bi[i+1] = ai[i], bi[i]
		if op.kind != '+' {
			ai[i+1]++
		}
		if op.kind != '-' {
			bi[i+1]++
		}
	}
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(0, i-DiffContext)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// A run of unchanged lines long enough closes the hunk.
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*DiffContext {
				end = min(len(ops), end+DiffContext)
				break
			}
			end = run
		}
		aCount, bCount := ai[end]-ai[start], bi[end]-bi[start]
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(ai[start], aCount), hunkRange(bi[start], bCount))
		for _, op := range ops[start:end] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffLines is an LCS diff after trimming the common prefix and suffix. Very
// large middles fall back to "replace it all", which is still a correct diff.
func diffLines(a, b []string) []diffOp {
	var pre, suf []diffOp
	for len(a) > 0 && len(b) > 0 && a[0] == b[0] {
		pre = append(pre, diffOp{' ', a[0]})
		a, b = a[1:], b[1:]
	}
	for len(a) > 0 && len(b) > 0 && a[len(a)-1] == b[len(b)-1] {
		suf = append([]diffOp{{' ', a[len(a)-1]}}, suf...)
		a, b = a[:len(a)-1], b[:len(b)-1]
	}
	var mid []diffOp
	if len(a)*len(b) > 4_000_000 {
		for _, l := range a {
			mid = append(mid, diffOp{'-', l})
		}
		for _, l := range b {
			mid = append(mid, diffOp{'+', l})
		}
	} else {
		n, m := len(a), len(b)
		dp := make([][]int, n+1)
		for i := range dp {
			dp[i] = make([]int, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if a[i] == b[j] {
					dp[i][j] = dp[i+1][j+1] + 1
				} else {
					dp[i][j] = max(dp[i+1][j], dp[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n && j < m {
			switch {
			case a[i] == b[j]:
				mid = append(mid, diffOp{' ', a[i]})
				i++
				j++
			case dp[i+1][j] >= dp[i][j+1]:
				mid = append(mid, diffOp{'-', a[i]})
				i++
			default:
				mid = append(mid, diffOp{'+', b[j]})
				j++
			}
		}
		for ; i < n; i++ {
			mid = append(mid, diffOp{'-', a[i]})
		}
		for ; j < m; j++ {
			mid = append(mid, diffOp{'+', b[j]})
		}
	}
	return append(append(pre, mid...), suf...)
}

// Diff colours a unified diff: additions ok, removals err, hunks accent.
func Diff(unified string) string {
	lines := strings.Split(strings.TrimSuffix(unified, "\n"), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			lines[i] = Bold(l)
		case strings.HasPrefix(l, "@@"):
			lines[i] = Accent(l)
		case strings.HasPrefix(l, "+"):
			lines[i] = OK(l)
		case strings.HasPrefix(l, "-"):
			lines[i] = Err(l)
		}
	}
	return strings.Join(lines, "\n")
}

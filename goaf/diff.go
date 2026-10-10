package main

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a minimal unified diff of old vs new line slices.
// context controls how many unchanged lines surround each change hunk.
func unifiedDiff(oldPath, newPath, oldText, newText string, context int) string {
	oldLines := splitDiffLines(oldText)
	newLines := splitDiffLines(newText)
	ops := diffOps(oldLines, newLines)
	changed := false
	for _, op := range ops {
		if op.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("--- %s\n+++ %s\n", oldPath, newPath))

	// Group ops into hunks separated by more than 2*context unchanged lines.
	i := 0
	for i < len(ops) {
		// Skip leading equal runs, keeping up to context lines.
		start := i
		for start < len(ops) && ops[start].kind == ' ' {
			start++
		}
		// Back up to include leading context.
		hs := start - context
		if hs < i {
			hs = i
		}
		// Find hunk end: last non-equal op + trailing context.
		end := start
		lastChange := start
		for end < len(ops) {
			if ops[end].kind != ' ' {
				lastChange = end
			}
			// Stop after 2*context+1 trailing equals (gap to next hunk).
			if end-lastChange > 2*context && ops[end].kind == ' ' {
				break
			}
			end++
		}
		he := lastChange + 1 + context
		if he > end {
			he = end
		}
		if he > len(ops) {
			he = len(ops)
		}

		// Compute hunk header ranges.
		oa, ob, na, nb := 0, 0, 0, 0
		for k := hs; k < he; k++ {
			switch ops[k].kind {
			case ' ':
				oa++
				ob++
				na++
				nb++
			case '-':
				oa++
				ob++
			case '+':
				na++
				nb++
			}
		}
		// Starting line numbers.
		os, ns := 1, 1
		for k := 0; k < hs; k++ {
			switch ops[k].kind {
			case ' ', '-':
				os++
			}
			switch ops[k].kind {
			case ' ', '+':
				ns++
			}
		}
		b.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", os, ob, ns, nb))
		_ = oa
		_ = na
		for k := hs; k < he; k++ {
			b.WriteString(string(ops[k].kind) + ops[k].line + "\n")
		}
		i = he
	}
	return strings.TrimRight(b.String(), "\n")
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// diffOps computes a line diff via LCS (fine for config-file sizes).
func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// lcs[i][j] = LCS length of a[i:] and b[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

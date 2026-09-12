package ui

// yamldiff.go — the YAML diff pane of the revisions view (#417): a
// deterministic line diff of the canonical spec YAML. Simple LCS over lines
// (templates are small documents; O(n·m) is fine at this size and the
// output is stable, which the golden-style tests pin).

// diffLineKind is the line vocabulary: same | added | removed.
type diffLineKind string

const (
	diffLineSame    diffLineKind = "same"
	diffLineAdded   diffLineKind = "added"
	diffLineRemoved diffLineKind = "removed"
)

type diffLine struct {
	Kind diffLineKind
	Text string
}

// lineDiff computes a side-by-side-able unified diff of two documents by
// lines (longest-common-subsequence backtrace), reusing the console's
// splitLines helper. Context lines around changes are NOT elided —
// template specs are short and the full document is the review artifact
// (Kestra FlowRevisions pattern).
func lineDiff(a, b string) []diffLine {
	x, y := splitLines(a), splitLines(b)
	n, m := len(x), len(y)
	// lcs[i][j] = LCS length of x[i:], y[j:]
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	out := make([]diffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, diffLine{diffLineSame, x[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffLine{diffLineRemoved, x[i]})
			i++
		default:
			out = append(out, diffLine{diffLineAdded, y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{diffLineRemoved, x[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{diffLineAdded, y[j]})
	}
	return out
}

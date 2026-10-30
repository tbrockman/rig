package agent

import (
	"fmt"
	"strings"
)

// UnifiedDiff is a line diff of a against b in the unified format (three
// lines of context), headed with the two names; "" when they are equal. It is
// what `rig agent brief` delivers: the agent gets the change in order with
// everything else the operator sent, and `rig brief` has the whole text.
//
// The common head and tail are trimmed before the middle is compared, which is
// where an edit to a brief lives. A middle too large to compare line by line
// (maxCells) is reported as replaced whole: still a correct diff, only not the
// shortest one.
func UnifiedDiff(nameA, nameB, a, b string) string {
	if a == b {
		return ""
	}
	x, y := splitLines(a), splitLines(b)
	ops := diffLines(x, y)

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", nameA, nameB)
	const context = 3
	// Group the edit script into hunks: runs of changes with at most
	// 2*context equal lines between them.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-context, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run == len(ops) || run-end > 2*context {
				end = min(end+context, len(ops))
				break
			}
			end = run
		}
		writeHunk(&out, ops[start:end])
		i = end
	}
	return out.String()
}

type diffOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	ai, bi int // 1-based line in a and in b this op is at
}

func writeHunk(out *strings.Builder, ops []diffOp) {
	var aStart, bStart, aLen, bLen int
	for _, op := range ops {
		if op.kind != '+' {
			if aLen == 0 {
				aStart = op.ai
			}
			aLen++
		}
		if op.kind != '-' {
			if bLen == 0 {
				bStart = op.bi
			}
			bLen++
		}
	}
	// An empty side is named by the line before it, as diff(1) does.
	if aLen == 0 {
		aStart = ops[0].ai - 1
	}
	if bLen == 0 {
		bStart = ops[0].bi - 1
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(aStart, aLen), hunkRange(bStart, bLen))
	for _, op := range ops {
		out.WriteByte(op.kind)
		out.WriteString(op.text)
		out.WriteByte('\n')
	}
}

func hunkRange(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// maxCells bounds the comparison table: a few megabytes at most.
const maxCells = 4 << 20

func diffLines(x, y []string) []diffOp {
	var ops []diffOp
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{' ', x[i], i + 1, i + 1})
	}
	mx, my := x[pre:len(x)-suf], y[pre:len(y)-suf]
	ai, bi := pre+1, pre+1
	emit := func(kind byte, text string) {
		ops = append(ops, diffOp{kind, text, ai, bi})
		if kind != '+' {
			ai++
		}
		if kind != '-' {
			bi++
		}
	}
	if (len(mx)+1)*(len(my)+1) > maxCells {
		for _, l := range mx {
			emit('-', l)
		}
		for _, l := range my {
			emit('+', l)
		}
	} else {
		// lcs[i][j] is the longest common subsequence of mx[i:] and my[j:].
		w := len(my) + 1
		lcs := make([]int32, (len(mx)+1)*w)
		for i := len(mx) - 1; i >= 0; i-- {
			for j := len(my) - 1; j >= 0; j-- {
				if mx[i] == my[j] {
					lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
				} else {
					lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
				}
			}
		}
		i, j := 0, 0
		for i < len(mx) || j < len(my) {
			switch {
			case i < len(mx) && j < len(my) && mx[i] == my[j]:
				emit(' ', mx[i])
				i, j = i+1, j+1
			case i < len(mx) && (j == len(my) || lcs[(i+1)*w+j] >= lcs[i*w+j+1]):
				emit('-', mx[i]) // removals first, as diff(1) prints them
				i++
			default:
				emit('+', my[j])
				j++
			}
		}
	}
	for k := len(x) - suf; k < len(x); k++ {
		emit(' ', x[k])
	}
	return ops
}

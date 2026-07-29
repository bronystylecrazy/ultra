package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Every column in this binary goes through here.
//
// The bug that forced it: `ultra fleet status` printed its rows with
// "%-32s %-12s …", and one product whose module path is 47 characters long
// pushed every later column of THAT row 15 places right — the table sheared,
// and the DRIFT column a fleet table exists to be scanned for stopped lining
// up with its header. A fixed width is a guess about data you have not seen
// yet; text/tabwriter measures instead.
//
// COLOUR: a padded column and an escape sequence cannot share a width —
// tabwriter counts the bytes of "\x1b[31mDRIFT\x1b[0m" as eleven characters of
// content nobody can see. The rule here is therefore mechanical, and it is the
// same rule everywhere: ANSI is allowed in the LAST cell of a row and nowhere
// else. tabwriter does not pad a trailing cell (it is not part of any aligned
// column), so colour there cannot move anything. row() enforces it.

// plainLine reduces text a padded cell cannot hold — a product's OWN
// diagnostic, which is coloured and multi-line — to text it can. It lives here
// rather than at its caller because it serves row()'s invariant: row panics on
// ANSI rather than shear a table, and this is how a caller obeys that rule.
func plainLine(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC: drop the whole CSI sequence, not just the ESC
			j := i + 1
			if j < len(s) && s[j] == '[' {
				for j++; j < len(s) && (s[j] < '@' || s[j] > '~'); j++ {
				}
				if j < len(s) {
					j++ // the final byte closes the sequence
				}
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	// Fields splits on spaces, tabs and newlines at once, so one pass flattens
	// a multi-line diagnostic and collapses the runs it leaves behind.
	return strings.Join(strings.Fields(b.String()), " ")
}

// table is one aligned block: rows in, columns out.
type table struct{ tw *tabwriter.Writer }

// newTable is the data-table constructor — a two-space gutter, the density a
// fleet listing wants.
func newTable(w io.Writer) *table { return &table{tw: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)} }

// newHelpTable is the help-screen constructor — a three-space gutter, so a
// command listing reads like every other Go CLI's.
func newHelpTable(w io.Writer) *table {
	return &table{tw: tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)}
}

// row writes one row. Any cell but the last must be plain text; see the ANSI
// note above. The check is a panic rather than a silent strip because a
// sheared table is a bug that reaches a user looking like bad data.
func (t *table) row(cells ...string) {
	for i, c := range cells {
		if i < len(cells)-1 && strings.Contains(c, "\x1b[") {
			panic("cmd/ultra: ANSI in a padded table cell — colour the last column only")
		}
	}
	fmt.Fprintln(t.tw, strings.Join(cells, "\t"))
}

func (t *table) flush() { t.tw.Flush() }

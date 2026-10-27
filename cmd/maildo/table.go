package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"golang.org/x/text/width"

	"github.com/vincentsch/maildo/internal/mail"
	"github.com/vincentsch/rungrad"
)

// now is the clock for relative dates. Tests replace it.
var now = time.Now

// table prints rows in aligned columns, like gh does. One column may be
// marked as flexible: it is cut short with "..." when the table would be
// wider than the screen.
type table struct {
	header []string
	rows   [][]string
	flex   int // index of the column that shrinks; -1 for none
	indent string
}

func newTable(header ...string) *table {
	return &table{header: header, flex: -1}
}

func (t *table) add(cells ...string) {
	for i := range cells {
		cells[i] = plainField(cells[i])
	}
	t.rows = append(t.rows, cells)
}

// render writes the table. maxWidth is the screen width; 0 means no limit.
func (t *table) render(w io.Writer, maxWidth int, bold func(string) string) {
	cols := len(t.header)
	for _, row := range t.rows {
		cols = max(cols, len(row))
	}
	widths := make([]int, cols)
	for i, h := range t.header {
		widths[i] = textWidth(h)
	}
	for _, row := range t.rows {
		for i := 0; i < cols && i < len(row); i++ {
			if n := textWidth(row[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}
	// The last column is not padded, so its width only matters for the
	// total.
	if t.flex >= 0 && maxWidth > 0 {
		total := len(t.indent) + 2*(cols-1)
		for _, n := range widths {
			total += n
		}
		if over := total - maxWidth; over > 0 {
			widths[t.flex] = max(widths[t.flex]-over, 12)
		}
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		b.WriteString(t.indent)
		for i := 0; i < cols; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			cell = truncate(cell, widths[i])
			if i < cols-1 {
				cell += strings.Repeat(" ", widths[i]-textWidth(cell)) + "  "
			}
			b.WriteString(cell)
		}
		out := strings.TrimRight(b.String(), " ")
		if style != nil {
			out = style(out)
		}
		fmt.Fprintln(w, out)
	}
	if len(t.header) > 0 {
		line(t.header, bold)
	}
	for _, row := range t.rows {
		line(row, nil)
	}
}

// textWidth counts screen columns: wide East Asian characters take two.
func textWidth(s string) int {
	n := 0
	for _, r := range s {
		switch width.LookupRune(r).Kind() {
		case width.EastAsianWide, width.EastAsianFullwidth:
			n += 2
		default:
			n++
		}
	}
	return n
}

// truncate cuts s to at most n screen columns, ending in "...".
func truncate(s string, n int) string {
	if textWidth(s) <= n {
		return s
	}
	if n <= 3 {
		return strings.Repeat(".", n)
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rw := textWidth(string(r))
		if used+rw > n-3 {
			break
		}
		b.WriteRune(r)
		used += rw
	}
	return strings.TrimRight(b.String(), " ") + "..."
}

// screenWidth is the terminal width when stdout is a terminal. Otherwise
// it is 120, or $COLUMNS when that is set.
func screenWidth(f *rungrad.Factory) int {
	if file, ok := f.Stdout.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		if w, _, err := term.GetSize(int(file.Fd())); err == nil && w > 20 {
			return w
		}
	}
	if v, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && v > 20 {
		return v
	}
	return 120
}

// boldStyle makes table headers bold on a color terminal.
func boldStyle(f *rungrad.Factory) func(string) string {
	mode := f.TerminalMode()
	if !mode.ANSI || !mode.Color {
		return nil
	}
	return func(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
}

// relativeTime says how long ago t was, in local time: "5 minutes ago",
// "yesterday", "3 days ago", then the date.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	current := now()
	d := current.Sub(t)
	local := t.Local()
	switch {
	case d < -time.Hour:
		return dateText(local, current)
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return countAgo(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return countAgo(int(d/time.Hour), "hour")
	}
	y1, m1, d1 := current.Local().AddDate(0, 0, -1).Date()
	if y, m, day := local.Date(); y == y1 && m == m1 && day == d1 {
		return "yesterday"
	}
	if d < 7*24*time.Hour {
		days := int(d / (24 * time.Hour))
		if days < 2 {
			days = 2
		}
		return countAgo(days, "day")
	}
	return dateText(local, current)
}

func countAgo(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

func dateText(t, current time.Time) string {
	if t.Year() == current.Local().Year() {
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2, 2006")
}

// senderName is the sender's name, or the address when there is no name.
func senderName(r mail.Recipient) string {
	if name := strings.TrimSpace(r.Name); name != "" {
		return name
	}
	if r.Address != "" {
		return r.Address
	}
	return "-"
}

// subjectText quotes a subject for a sentence, or says it has none.
func subjectText(subject string) string {
	subject = strings.TrimSpace(plainField(subject))
	if subject == "" {
		return "(no subject)"
	}
	return quote(subject)
}

// quote puts double quotes around a name or subject for people. Unlike %q
// it leaves quotes and backslashes inside alone; plainField has already
// removed control characters.
func quote(s string) string {
	return "\"" + s + "\""
}

// thousands writes 2512 as 2,512.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 1000 && n > -1000 {
		return s
	}
	var b strings.Builder
	start := len(s) % 3
	if s[0] == '-' {
		b.WriteByte('-')
		s = s[1:]
		start = len(s) % 3
	}
	if start > 0 {
		b.WriteString(s[:start])
	}
	for i := start; i < len(s); i += 3 {
		if b.Len() > 0 && b.String() != "-" {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return thousands(n) + " " + many
}

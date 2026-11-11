// Adapted from zbxctl (github.com/mwtrigg/zbxctl, internal/output): port fixes both ways.

package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// Exit codes.
const (
	ExitSuccess    = 0
	ExitError      = 1
	ExitNotFound   = 2
	ExitAuthError  = 3
	ExitConnect    = 4 // cannot reach the controller
	ExitUsage      = 5 // invalid arguments or flags
	ExitVersion    = 6 // the controller is older than awxctl supports
	ExitPermission = 8 // the context's permission tier does not allow the command
	ExitUnknown    = 7 // a write was sent but its outcome is not known (timeout, 502/504): check before retrying
)

// Fields, when set, keeps only these fields of each row, in this order, in every
// format: an agent that needs four fields does not pay for forty.
var Fields []string

// Zone, when set, is the time zone timestamps are shown in (the API gives UTC).
var Zone *time.Location

// project cuts rows and columns down to Fields. A name no row has is an error
// that lists what there is, unless there are no rows to judge by.
func project(columns []Column, rows []map[string]any) ([]Column, []map[string]any, error) {
	if len(rows) > 0 {
		for _, f := range Fields {
			known := false
			for _, row := range rows {
				if _, ok := row[f]; ok {
					known = true
					break
				}
			}
			if !known {
				keys := make([]string, 0, len(rows[0]))
				for k := range rows[0] {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				return nil, nil, &FieldError{Msg: fmt.Sprintf("no such field %q (available: %s)", f, strings.Join(keys, ", "))}
			}
		}
	}
	header := map[string]string{}
	for _, c := range columns {
		header[c.Key] = c.Header
	}
	cols := make([]Column, len(Fields))
	for i, f := range Fields {
		h := header[f]
		if h == "" {
			h = strings.ToUpper(f)
		}
		cols[i] = Column{Key: f, Header: h}
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		m := make(map[string]any, len(Fields))
		for _, f := range Fields {
			m[f] = row[f]
		}
		out[i] = m
	}
	return cols, out, nil
}

// inZone returns the rows with every timestamp (a text in RFC 3339 form) moved to
// the zone, at any depth. Other values are untouched.
func inZone(rows []map[string]any, zone *time.Location) []map[string]any {
	var conv func(v any) any
	conv = func(v any) any {
		switch t := v.(type) {
		case string:
			if len(t) >= 20 && len(t) <= 35 && t[4] == '-' && t[10] == 'T' {
				if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
					return ts.In(zone).Format("2006-01-02T15:04:05Z07:00")
				}
			}
			return t
		case map[string]any:
			m := make(map[string]any, len(t))
			for k, x := range t {
				m[k] = conv(x)
			}
			return m
		case []any:
			l := make([]any, len(t))
			for i, x := range t {
				l[i] = conv(x)
			}
			return l
		case []map[string]any:
			l := make([]map[string]any, len(t))
			for i, x := range t {
				l[i] = conv(x).(map[string]any)
			}
			return l
		case []string:
			l := make([]string, len(t))
			for i, x := range t {
				l[i], _ = conv(x).(string)
			}
			return l
		}
		return v
	}
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		out[i] = conv(r).(map[string]any)
	}
	return out
}

// Raw includes the raw API values (*_raw, *_unix) next to the readable ones.
var Raw bool

// Field, when set, makes Print emit just that field of each row, one value
// per line (list values one element per line).
var Field string

// Command is the command path reported in the JSON envelope's meta.
var Command string

// Limit is the result limit in force for a list command (0 = none). When a list
// has that many rows the output says so, since there may be more.
var Limit int

// Total is how many objects match on the server, when a list command knows it
// (0 = not known). It can be more than the rows printed.
var Total int

// TotalKnown says that Total was set by a list command, so a total of 0 is
// reported too ("how many failed jobs": none).
var TotalKnown bool

// Fetched is the number of rows the API returned, when that is not the number of rows
// printed (grouped output prints groups, not the rows that were limited). 0 = use the
// printed rows.
var Fetched int

func limitReached(rows []map[string]any) bool {
	n := len(rows)
	if Fetched > 0 {
		n = Fetched
	}
	return Limit > 0 && n >= Limit
}

// KindFor maps an exit code to a stable machine-readable error kind.
func KindFor(code int) string {
	switch code {
	case ExitNotFound:
		return "not_found"
	case ExitAuthError:
		return "auth"
	case ExitConnect:
		return "connection"
	case ExitUsage:
		return "usage"
	case ExitVersion:
		return "unsupported_version"
	case ExitUnknown:
		return "unknown_outcome"
	case ExitPermission:
		return "permission"
	default:
		return "error"
	}
}

// stripRaw drops the raw duplicates unless --raw was given.
func stripRaw(rows []map[string]any) []map[string]any {
	if Raw {
		return rows
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		m := make(map[string]any, len(row))
		for k, v := range row {
			if strings.HasSuffix(k, "_raw") || strings.HasSuffix(k, "_unix") {
				continue
			}
			m[k] = v
		}
		out[i] = m
	}
	return out
}

// Format is the output format.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatJSONL Format = "jsonl"
	FormatCSV   Format = "csv"
)

// Column describes a single output column.
type Column struct {
	Key    string // map key
	Header string // display name
}

// Print writes rows to stdout in the requested format.
func Print(format Format, columns []Column, rows []map[string]any) error {
	if Zone != nil {
		rows = inZone(rows, Zone)
	}
	if Field != "" {
		return printField(rows)
	}
	rows = stripRaw(rows)
	columns = visibleColumns(columns)
	if len(Fields) > 0 {
		var err error
		if columns, rows, err = project(columns, rows); err != nil {
			return err
		}
	}
	if format != FormatJSON && limitReached(rows) {
		fmt.Fprintf(os.Stderr, "note: showing %d rows, the limit; there may be more (raise --limit, or --limit -1 for all)\n", Limit)
	}
	switch format {
	case FormatJSON:
		return printJSON(columns, rows)
	case FormatJSONL:
		return printJSONL(columns, rows)
	case FormatCSV:
		return printCSV(columns, rows)
	default:
		return printTable(columns, rows)
	}
}

func printTable(columns []Column, rows []map[string]any) error {
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)

	// Minimal style: header separator, no box borders, space-separated columns.
	style := table.Style{
		Name: "awxctl",
		Box: table.BoxStyle{
			PaddingLeft:      " ",
			PaddingRight:     " ",
			MiddleVertical:   " ",
			MiddleSeparator:  " ",
			MiddleHorizontal: "-",
		},
		Format: table.FormatOptions{
			Header: text.FormatUpper,
		},
		Options: table.Options{
			DrawBorder:      false,
			SeparateColumns: false,
			SeparateHeader:  true,
			SeparateRows:    false,
		},
	}
	t.SetStyle(style)

	// Header row.
	headerRow := make(table.Row, len(columns))
	for i, c := range columns {
		headerRow[i] = c.Header
	}
	t.AppendHeader(headerRow)

	// Data rows.
	for _, row := range rows {
		r := make(table.Row, len(columns))
		for i, c := range columns {
			r[i] = safeCell(cell(row[c.Key]))
		}
		t.AppendRow(r)
	}

	t.Render()
	return nil
}

func printJSON(_ []Column, rows []map[string]any) error {
	meta := map[string]any{
		"count":   len(rows),
		"command": Command,
	}
	if Total > 0 || TotalKnown {
		meta["total"] = Total // matching objects on the server; count is the rows returned here
	}
	if Limit > 0 {
		meta["limit"] = Limit
		meta["limit_reached"] = limitReached(rows) // true: there may be more rows; raise --limit (-1 for all)
	}
	envelope := map[string]any{"data": rows, "meta": meta}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(envelope)
}

func printJSONL(_ []Column, rows []map[string]any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

// filterColumns returns a new slice of maps containing only the specified columns.
func filterColumns(columns []Column, rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		m := make(map[string]any, len(columns))
		for _, c := range columns {
			m[c.Key] = row[c.Key]
		}
		out[i] = m
	}
	return out
}

// PrintError writes a structured error to stderr.
func PrintError(format Format, msg string, code int) {
	switch format {
	case FormatJSON, FormatJSONL:
		enc := json.NewEncoder(os.Stderr)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]any{"error": msg, "kind": KindFor(code), "code": code})
	default:
		fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	}
}

// DefaultFormat is JSON: agents are the primary users. Use -o table for humans.
func DefaultFormat() Format { return FormatJSON }

// cell renders list values compactly for table output.
func cell(v any) any {
	if v == nil {
		return "-"
	}
	if l, ok := v.([]string); ok {
		if len(l) == 0 {
			return "-"
		}
		return strings.Join(l, ",")
	}
	return v
}

// Printable makes text safe for a terminal: control characters, including ESC and
// carriage return, which can rewrite or hide what was printed before, are shown as
// visible \xNN escapes. Newline and tab are kept. API strings (job names,
// item values) are written by third parties and are shown raw in tables otherwise.
func Printable(s string) string {
	clean := true
	for _, r := range s {
		if unsafeRune(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !unsafeRune(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			fmt.Fprintf(&b, "\\u%04x", r)
		}
	}
	return b.String()
}

// unsafeRune reports characters that change how surrounding text is displayed or that
// are invisible: C0 and C1 controls (except newline and tab), and the Unicode format
// characters that reorder text (bidi marks, embeddings, overrides and isolates), join or
// break it invisibly (zero-width space, joiners, word joiner, BOM) or end a line (U+2028/9).
func unsafeRune(r rune) bool {
	switch {
	case r < 0x20:
		return r != '\n' && r != '\t'
	case r == 0x7f, r >= 0x80 && r < 0xa0:
		return true
	case r >= 0x200b && r <= 0x200f, // zero-width space/joiners, LRM, RLM
		r >= 0x2028 && r <= 0x202e, // line/paragraph separators, bidi embeddings and overrides
		r >= 0x2060 && r <= 0x2064, // word joiner and invisible operators
		r >= 0x2066 && r <= 0x2069, // bidi isolates
		r == 0xfeff, r == 0x061c, r == 0x180e:
		return true
	}
	return false
}

func safeCell(v any) any {
	if s, ok := v.(string); ok {
		return Printable(s)
	}
	return v
}

// FieldError is returned when --field names a field that does not exist.
type FieldError struct{ Msg string }

func (e *FieldError) Error() string { return e.Msg }

func printField(rows []map[string]any) error {
	for _, row := range rows {
		v, ok := row[Field]
		if !ok {
			keys := make([]string, 0, len(row))
			for k := range row {
				if !strings.HasSuffix(k, "_raw") && !strings.HasSuffix(k, "_unix") {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			return &FieldError{fmt.Sprintf("no such field %q (available: %s)", Field, strings.Join(keys, ", "))}
		}
		switch t := v.(type) {
		case nil:
		case []string:
			for _, e := range t {
				fmt.Println(e)
			}
		default:
			fmt.Println(t)
		}
	}
	return nil
}

func printCSV(columns []Column, rows []map[string]any) error {
	w := csv.NewWriter(os.Stdout)
	head := make([]string, len(columns))
	for i, c := range columns {
		head[i] = c.Key
	}
	if err := w.Write(head); err != nil {
		return err
	}
	for _, row := range rows {
		rec := make([]string, len(columns))
		for i, c := range columns {
			switch v := row[c.Key].(type) {
			case nil:
			case []string:
				rec[i] = strings.Join(v, ";")
			default:
				rec[i] = fmt.Sprint(v)
			}
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

// visibleColumns drops the raw-value columns unless --raw was given, so tables
// and CSV do not show empty columns for fields that were stripped.
func visibleColumns(cols []Column) []Column {
	if Raw {
		return cols
	}
	out := make([]Column, 0, len(cols))
	for _, c := range cols {
		if strings.HasSuffix(c.Key, "_raw") || strings.HasSuffix(c.Key, "_unix") {
			continue
		}
		out = append(out, c)
	}
	return out
}

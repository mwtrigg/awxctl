package cli

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mwtrigg/awxctl/internal/core"
	"github.com/mwtrigg/awxctl/internal/output"
)

var getCmd = group(&cobra.Command{
	Use:   "get",
	Short: "Read objects: jobs, templates, inventories, hosts, projects, schedules, ...",
	Long: `Read objects from the controller. Every kind takes the same flags:

  awxctl get jobs --failed --since 24h
  awxctl get hosts --inventory lab --name web
  awxctl get job-templates "Deploy app"        # one object, by name or ID (id:12)

--filter passes any API filter through (repeatable), for example
--filter status=failed --filter name__icontains=patch.`,
})

// listing is true while a list (not a single object) is being printed.
var listing bool

var (
	getFilters []string
	getSearch  string
	getName    string
	getSince   string
	getUntil   string
	getFailed  bool
	getLimit   int
	getOrder   string
	getWide    bool
	getGroupBy string
)

// columns returns the output columns of a resource; wide fields only when asked.
func columns(r *core.Resource, wide bool) []output.Column {
	var cols []output.Column
	for _, fd := range r.Fields {
		if fd.Wide && !wide {
			continue
		}
		if fd.Detail && !getWide && wide && r.Kind != "" && listing {
			continue // a list in JSON: the large fields stay out unless --wide
		}
		cols = append(cols, output.Column{Key: fd.Key, Header: fd.Header})
	}
	return cols
}

// parseWhen reads a relative time ("30m", "24h", "7d", "2w": that long ago) or an
// absolute one (RFC 3339, or a date) and returns it as the API wants it.
func parseWhen(s string, now time.Time) (string, error) {
	s = strings.TrimSpace(s)
	if n := len(s); n > 1 {
		if v, err := strconv.ParseFloat(s[:n-1], 64); err == nil && v >= 0 {
			unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[s[n-1]]
			if unit != 0 {
				return now.Add(-time.Duration(v * float64(unit))).UTC().Format(time.RFC3339), nil
			}
		}
	}
	loc := time.UTC // a time written without a zone is in --tz when that is set, else UTC
	if output.Zone != nil {
		loc = output.Zone
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UTC().Format(time.RFC3339), nil
		}
	}
	return "", &usageError{fmt.Sprintf("cannot read the time %q: use 30m, 24h, 7d, a date (2026-01-31) or RFC 3339", s)}
}

// listQuery builds the API query of a list command from the common flags.
func listQuery(cmd *cobra.Command, r *core.Resource) (url.Values, error) {
	q := url.Values{}
	for _, kv := range getFilters {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, &usageError{fmt.Sprintf("--filter wants KEY=VALUE, got %q", kv)}
		}
		if k == "page" || k == "page_size" || k == "format" {
			return nil, &usageError{"--filter " + k + " is not a filter: use --limit"}
		}
		q.Add(k, v)
	}
	for flag, param := range r.Filters {
		if fl := cmd.Flags().Lookup(flag); fl != nil && fl.Changed {
			q.Set(param, fl.Value.String())
		}
	}
	if getSearch != "" {
		q.Set("search", getSearch)
	}
	if getName != "" {
		if r.NameField == "" {
			q.Set("name__icontains", getName)
		} else {
			q.Set(r.NameField+"__icontains", getName)
		}
	}
	if getFailed {
		q.Set("failed", "true")
	}
	for flag, op := range map[string]string{"since": "__gte", "until": "__lte"} {
		val := map[string]string{"since": getSince, "until": getUntil}[flag]
		if val == "" {
			continue
		}
		if r.TimeField == "" {
			return nil, &usageError{fmt.Sprintf("--%s does not apply to %s", flag, r.Kind)}
		}
		t, err := parseWhen(val, time.Now())
		if err != nil {
			return nil, err
		}
		q.Set(r.TimeField+op, t)
	}
	order := r.Order
	if getOrder != "" {
		order = getOrder
	}
	if order != "" {
		q.Set("order_by", order)
	}
	return q, nil
}

// effectiveLimit resolves --limit: unset = the context default, -1 = everything.
func effectiveLimit(cmd *cobra.Command, def int) int {
	if !cmd.Flags().Changed("limit") {
		return def
	}
	if getLimit < 0 {
		return 0
	}
	return getLimit
}

func runGet(r *core.Resource) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx, _, client, err := connect()
		if err != nil {
			return err
		}
		outFmt := output.Format(flagOutput)
		if len(args) == 1 { // one object: every field
			extra := url.Values{}
			if r.Scope != "" {
				if fl := cmd.Flags().Lookup(r.Scope); fl != nil && fl.Changed {
					extra.Set(r.Filters[r.Scope], fl.Value.String())
				}
			}
			o, err := client.Resolve(cmd.Context(), r, args[0], extra)
			if err != nil {
				return err
			}
			row := r.Row(o)
			cols := columns(r, true)
			// Variables can run to pages and are rarely what was asked: they have
			// their own command. --wide still shows them here.
			if _, has := row["variables"]; has && !getWide {
				delete(row, "variables")
				kept := cols[:0]
				for _, c := range cols {
					if c.Key != "variables" {
						kept = append(kept, c)
					}
				}
				cols = kept
				row["variables_hint"] = "left out: awxctl get variables " + r.Kind + " " + strconv.Quote(args[0]) + " (hosts: add --merged)"
			}
			if r.Kind == "inventories" { // one inventory: say where its hosts come from
				sources, err := client.InventorySources(cmd.Context(), o)
				if err != nil {
					return err
				}
				row["sources"] = sources
				names := make([]string, len(sources))
				for i, s := range sources {
					names[i] = fmt.Sprintf("%v (%v)", s["name"], s["source"])
				}
				if outFmt == output.FormatTable {
					row["sources"] = strings.Join(names, ", ")
				}
				cols = append(cols, output.Column{Key: "sources", Header: "SOURCES FROM"})
			}
			return output.Print(outFmt, cols, []map[string]any{row})
		}
		q, err := listQuery(cmd, r)
		if err != nil {
			return err
		}
		listing = true
		defer func() { listing = false }()
		if cmd.Flags().Changed("limit") && getLimit == 0 {
			return &usageError{"--limit 0 is not a limit: use --limit 1 and read meta.total for a count, or --limit -1 for everything"}
		}
		limit := effectiveLimit(cmd, ctx.DefaultLimit)
		objs, total, err := client.List(cmd.Context(), r.Path, q, limit)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, len(objs))
		for i, o := range objs {
			rows[i] = r.ListRow(o, getWide) // large fields (variables, changes) only with --wide
		}
		output.Limit, output.Total, output.TotalKnown = limit, total, true
		if total > len(rows) {
			output.Fetched = limit // there are more than were read
		}
		if getGroupBy != "" {
			return printGroups(outFmt, r, rows, getGroupBy)
		}
		return output.Print(outFmt, columns(r, getWide || outFmt != output.FormatTable), rows)
	}
}

// printGroups counts the rows per value of one field: "failed jobs by template".
func printGroups(outFmt output.Format, r *core.Resource, rows []map[string]any, field string) error {
	known := false
	for _, fd := range r.Fields {
		known = known || fd.Key == field
	}
	if !known {
		return &usageError{fmt.Sprintf("--group-by %s: %s have no such field", field, r.Kind)}
	}
	type group struct {
		count, failed int
		newest        any
	}
	groups := map[string]*group{}
	var order []string
	for _, row := range rows {
		key := fmt.Sprint(row[field])
		if row[field] == nil {
			key = "(none)"
		}
		g := groups[key]
		if g == nil {
			g = &group{newest: row["id"]}
			groups[key] = g
			order = append(order, key)
		}
		g.count++
		if row["failed"] == true {
			g.failed++
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return groups[order[i]].count > groups[order[j]].count })
	out := make([]map[string]any, len(order))
	for i, key := range order {
		g := groups[key]
		out[i] = map[string]any{field: key, "count": g.count, "newest_id": g.newest}
		if r.JobLike {
			out[i]["failed"] = g.failed
		}
	}
	if output.Fetched == 0 {
		output.Fetched = len(rows) // the limit applies to the rows read, not to the groups
	}
	cols := []output.Column{{Key: field, Header: strings.ToUpper(field)}, {Key: "count", Header: "COUNT"}, {Key: "failed", Header: "FAILED"}, {Key: "newest_id", Header: "NEWEST ID"}}
	return output.Print(outFmt, cols, out)
}

func addListFlags(c *cobra.Command) {
	c.Flags().StringVar(&getGroupBy, "group-by", "", "count the rows per value of this field instead of listing them, e.g. --group-by template")
	c.Flags().StringArrayVar(&getFilters, "filter", nil, "API filter KEY=VALUE, passed through (repeatable), e.g. status=failed or name__icontains=web")
	c.Flags().StringVar(&getSearch, "search", "", "free-text search, as in the web UI")
	c.Flags().IntVar(&getLimit, "limit", 0, "most rows to return (default: the context's default_limit, 100); -1 = all")
	c.Flags().StringVar(&getOrder, "order", "", "sort field, prefix - for descending, e.g. -finished")
	c.Flags().BoolVar(&getWide, "wide", false, "every field: more table columns, and the large fields (variables, extra_vars, changes) that a list leaves out; prefer --fields")
}

func init() {
	for _, r := range core.Resources() {
		c := &cobra.Command{
			Use:     r.Kind + " [name-or-id]",
			Aliases: r.Aliases,
			Short:   "List " + r.Kind + ", or show one " + r.Singular,
			Args:    cobra.MaximumNArgs(1),
			RunE:    runGet(r),
		}
		addListFlags(c)
		c.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
			var own []string
			cmd.LocalFlags().VisitAll(func(fl *pflag.Flag) {
				if fl.Name != "help" {
					own = append(own, "--"+fl.Name)
				}
			})
			return &usageError{fmt.Sprintf("%v: %s takes %s; any other API filter goes in --filter KEY=VALUE", err, cmd.CommandPath(), strings.Join(own, ", "))}
		})
		if r.TimeField != "" {
			c.Flags().StringVar(&getSince, "since", "", "only those newer than this: 30m, 24h, 7d, a date or RFC 3339")
			c.Flags().StringVar(&getUntil, "until", "", "only those older than this (same forms as --since)")
		}
		if r.JobLike {
			c.Flags().BoolVar(&getFailed, "failed", false, "only those that failed")
		}
		if r.JobLike || r.NameField != "" {
			c.Flags().StringVar(&getName, "name", "", "name contains this text (case-insensitive)")
		}
		flags := make([]string, 0, len(r.Filters))
		for flag := range r.Filters {
			flags = append(flags, flag)
		}
		sort.Strings(flags)
		for _, flag := range flags {
			c.Flags().String(flag, "", "only those whose "+strings.ReplaceAll(flag, "-", " ")+" is this (exact)")
		}
		getCmd.AddCommand(c)
	}
	rootCmd.AddCommand(getCmd)
}

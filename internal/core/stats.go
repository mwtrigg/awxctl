package core

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// RunStats summarizes a group of runs: how many, how they ended and how long
// they took.
type RunStats struct {
	Key        string
	Runs       int
	Successful int
	Failed     int
	Canceled   int
	Running    int
	Durations  []float64 // seconds, of finished runs
	LastID     int
	LastStatus string
	LastRun    string
}

// Row is the flat form: counts, failure rate and runtime figures in seconds.
func (s *RunStats) Row(keyName string) map[string]any {
	row := map[string]any{keyName: s.Key, "runs": s.Runs, "successful": s.Successful, "failed": s.Failed, "canceled": s.Canceled, "running": s.Running,
		"last_id": s.LastID, "last_status": s.LastStatus, "last_run": s.LastRun}
	if done := s.Successful + s.Failed; done > 0 {
		row["failure_rate"] = math.Round(float64(s.Failed)/float64(done)*1000) / 1000
	}
	if n := len(s.Durations); n > 0 {
		d := append([]float64(nil), s.Durations...)
		sort.Float64s(d)
		sum := 0.0
		for _, v := range d {
			sum += v
		}
		round := func(v float64) float64 { return math.Round(v*10) / 10 }
		row["seconds_min"], row["seconds_max"] = round(d[0]), round(d[n-1])
		row["seconds_avg"], row["seconds_median"] = round(sum/float64(n)), round(percentile(d, 50))
		row["seconds_p95"], row["seconds_total"] = round(percentile(d, 95)), round(sum)
	}
	return row
}

// percentile of sorted values, nearest rank.
func percentile(sorted []float64, p float64) float64 {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// GroupRuns groups run rows (as a resource's Row gives them) by one field. "day"
// groups by the date the run started. Rows must be newest first.
func GroupRuns(rows []map[string]any, by string) []*RunStats {
	groups := map[string]*RunStats{}
	var order []string
	for _, row := range rows {
		key := "(none)"
		if by == "day" {
			if t, _ := row["started"].(string); len(t) >= 10 {
				key = t[:10]
			} else if t, _ := row["created"].(string); len(t) >= 10 {
				key = t[:10]
			}
		} else if row[by] != nil && row[by] != "" {
			key = fmt.Sprint(row[by])
		}
		g := groups[key]
		if g == nil {
			g = &RunStats{Key: key, LastID: asInt(row["id"])}
			g.LastStatus, _ = row["status"].(string)
			g.LastRun, _ = row["started"].(string)
			groups[key] = g
			order = append(order, key)
		}
		g.Runs++
		status, _ := row["status"].(string)
		switch status {
		case "successful":
			g.Successful++
		case "failed", "error":
			g.Failed++
		case "canceled":
			g.Canceled++
		default:
			g.Running++
		}
		if e, ok := row["elapsed"].(float64); ok && Finished(status) && status != "canceled" {
			g.Durations = append(g.Durations, e)
		}
	}
	out := make([]*RunStats, len(order))
	for i, k := range order {
		out[i] = groups[k]
	}
	if by == "day" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Key > out[j].Key })
	} else {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Runs > out[j].Runs })
	}
	return out
}

// HostStats counts, per host of an inventory, the jobs that ran against it in a
// time window and how the host fared. It asks the controller once per host, so
// maxHosts bounds the work; truncated reports that there were more hosts.
func (c *Client) HostStats(ctx context.Context, inventory Object, since string, maxHosts int) (rows []map[string]any, truncated bool, err error) {
	hosts, total, err := c.List(ctx, "/inventories/"+idOf(inventory)+"/hosts/", url.Values{"order_by": {"name"}}, maxHosts)
	if err != nil {
		return nil, false, err
	}
	for _, h := range hosts {
		q := url.Values{"order_by": {"-id"}}
		if since != "" {
			q.Set("created__gte", since)
		}
		sums, _, err := c.List(ctx, "/hosts/"+idOf(h)+"/job_host_summaries/", q, 0)
		if err != nil {
			return nil, false, err
		}
		row := map[string]any{"host": h["name"], "host_id": ID(h), "enabled": h["enabled"], "jobs": len(sums), "failed": 0, "unreachable": 0, "changed": 0}
		templates := map[string]bool{}
		seconds := 0.0
		for i, s := range sums {
			if s["failed"] == true {
				row["failed"] = row["failed"].(int) + 1
			}
			if asInt(s["dark"]) > 0 {
				row["unreachable"] = row["unreachable"].(int) + 1
			}
			if asInt(s["changed"]) > 0 {
				row["changed"] = row["changed"].(int) + 1
			}
			if name, _ := Lookup(s, "summary_fields.job.job_template_name").(string); name != "" {
				templates[name] = true
			}
			if e, ok := Lookup(s, "summary_fields.job.elapsed").(float64); ok {
				seconds += e
			}
			if i == 0 {
				row["last_job_id"], row["last_job_status"], row["last_job"] = Lookup(s, "summary_fields.job.id"), Lookup(s, "summary_fields.job.status"), s["created"]
				row["last_job_host_failed"] = s["failed"]
			}
		}
		row["templates"] = len(templates)
		row["job_seconds_total"] = math.Round(seconds*10) / 10
		rows = append(rows, row)
	}
	return rows, total > len(hosts), nil
}

// LatestHostJob finds the newest job that ran against a host, optionally only of
// one job template.
func (c *Client) LatestHostJob(ctx context.Context, host Object, template string, failedOnly bool) (int, error) {
	q := url.Values{"order_by": {"-id"}}
	if template != "" {
		q.Set("job__job_template__name", template)
	}
	if failedOnly {
		q.Set("failed", "true")
	}
	sums, _, err := c.List(ctx, "/hosts/"+idOf(host)+"/job_host_summaries/", q, 1)
	if err != nil {
		return 0, err
	}
	if len(sums) == 0 {
		what := "no job"
		if failedOnly {
			what = "no job in which the host failed"
		}
		if template != "" {
			what += fmt.Sprintf(" of template %q", template)
		}
		return 0, fmt.Errorf("%s has run against host %q: %w", what, host["name"], ErrNotFound)
	}
	return asInt(Lookup(sums[0], "summary_fields.job.id")), nil
}

// HostOutput returns what one job printed for one host: each task the host took
// part in, with the host's result. failedOnly keeps only the failed results.
func (c *Client) HostOutput(ctx context.Context, jobID int, host string, failedOnly bool) (text string, tasks int, err error) {
	q := url.Values{"host_name": {host}, "order_by": {"counter"}, "event__startswith": {"runner_"}}
	if failedOnly {
		q.Set("failed", "true")
	}
	evs, _, err := c.List(ctx, "/jobs/"+strconv.Itoa(jobID)+"/job_events/", q, 0)
	if err != nil {
		return "", 0, err
	}
	var b strings.Builder
	last := ""
	for _, e := range evs {
		out, _ := e["stdout"].(string)
		out = strings.TrimRight(strings.ReplaceAll(StripANSI(out), "\r", ""), "\n")
		if out == "" {
			continue // start and retry markers print nothing
		}
		task, _ := e["task"].(string)
		if key := fmt.Sprint(e["play"], "/", task); key != last {
			fmt.Fprintf(&b, "TASK [%s]\n", task)
			last = key
			tasks++
		}
		b.WriteString(out)
		b.WriteString("\n")
	}
	return b.String(), tasks, nil
}

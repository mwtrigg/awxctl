package core

import (
	"math"
	"sort"
	"time"
)

// Timing views: where a run's time went, from its task results. A task result
// carries how long the task took on one host. From those: per task, how long
// the task took in all (wall clock, first start to last end), how much host time
// it used, and which host was slowest; the same per play and per host.

// TimingEvents are the events a timing view is built from: the task results.
const TimingEvents = "runner_on_ok,runner_on_failed,runner_on_unreachable,runner_on_skipped"

type span struct {
	start, end time.Time
	known      bool
}

func (s *span) add(start, end time.Time) {
	if start.IsZero() || end.IsZero() {
		return
	}
	if !s.known || start.Before(s.start) {
		s.start = start
	}
	if !s.known || end.After(s.end) {
		s.end = end
	}
	s.known = true
}

func (s *span) seconds() any {
	if !s.known {
		return nil
	}
	return round3(s.end.Sub(s.start).Seconds())
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

func eventTime(o Object, key string) time.Time {
	s, _ := Lookup(o, "event_data."+key).(string)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

type timing struct {
	row                map[string]any
	span               span
	hosts, tasks       map[string]bool
	total, slowest     float64
	slowestOf          string
	changed, unchanged int
	failed, skipped    int
	unreachable        int
	first              int
}

func (t *timing) add(o Object, secs float64, slowestName string) {
	t.span.add(eventTime(o, "start"), eventTime(o, "end"))
	t.total += secs
	if secs > t.slowest || t.slowestOf == "" {
		t.slowest, t.slowestOf = secs, slowestName
	}
	switch o["event"] {
	case "runner_on_failed":
		t.failed++
	case "runner_on_unreachable":
		t.unreachable++
	case "runner_on_skipped":
		t.skipped++
	default:
		if o["changed"] == true {
			t.changed++
		} else {
			t.unchanged++
		}
	}
}

// Timing folds task results into one row per task, play or host ("task",
// "play", "host"). Tasks and plays come in the order they ran; hosts with the
// most host time first.
//
// wall_seconds is the time from the first start to the last end among the
// results (nil when the controller's events carry no start and end);
// host_seconds is the sum over hosts, which is more than the wall clock when
// hosts run side by side.
func Timing(evs []Object, by string) []map[string]any {
	groups := map[string]*timing{}
	var order []string
	for i, o := range evs {
		host, _ := o["host_name"].(string)
		task, _ := o["task"].(string)
		play, _ := o["play"].(string)
		secs, _ := eventSeconds(o).(float64)
		var key, slowestName string
		switch by {
		case "host":
			key, slowestName = host, task
		case "play":
			key, slowestName = asString(Lookup(o, "event_data.play_uuid"))+"\x00"+play, host
		default:
			key, slowestName = asString(Lookup(o, "event_data.task_uuid"))+"\x00"+play+"\x00"+task, host
		}
		g := groups[key]
		if g == nil {
			g = &timing{row: map[string]any{}, hosts: map[string]bool{}, tasks: map[string]bool{}, first: i}
			switch by {
			case "host":
				g.row["host"] = host
			case "play":
				g.row["play"] = play
			default:
				g.row["play"], g.row["task"] = play, task
				if role, _ := o["role"].(string); role != "" {
					g.row["role"] = role
				}
			}
			groups[key] = g
			order = append(order, key)
		}
		g.hosts[host] = true
		g.tasks[asString(Lookup(o, "event_data.task_uuid"))+"\x00"+task] = true
		g.add(o, secs, slowestName)
	}
	if by == "host" {
		sort.SliceStable(order, func(i, j int) bool { return groups[order[i]].total > groups[order[j]].total })
	}
	rows := make([]map[string]any, 0, len(order))
	for _, key := range order {
		g := groups[key]
		r := g.row
		r["wall_seconds"], r["host_seconds"], r["slowest_seconds"] = g.span.seconds(), round3(g.total), round3(g.slowest)
		r["changed"], r["unchanged"], r["failed"], r["skipped"], r["unreachable"] = g.changed, g.unchanged, g.failed, g.skipped, g.unreachable
		if g.span.known {
			r["started"] = g.span.start.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		if by == "host" {
			r["tasks"], r["slowest_task"] = len(g.tasks), g.slowestOf
		} else {
			r["hosts"], r["slowest_host"] = len(g.hosts), g.slowestOf
			if by == "play" {
				r["tasks"] = len(g.tasks)
			}
		}
		rows = append(rows, r)
	}
	return rows
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

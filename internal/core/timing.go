package core

import (
	"math"
	"sort"
	"strings"
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

func (t *timing) add(o Object, secs float64, slowestName string, itemChanged bool) {
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
		// A loop reports per item: the task's own result can say "not changed"
		// although items did change something. Any changed item makes it changed.
		if o["changed"] == true || itemChanged {
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
// itemsChanged are the results of loop items that changed something: a looped
// task counts as changed on a host when one of its items did, whatever the
// task's own result says.
//
// wall_seconds is the time from the first start to the last end among the
// results (nil when the controller's events carry no start and end);
// host_seconds is the sum over hosts, which is more than the wall clock when
// hosts run side by side.
func Timing(evs, itemsChanged []Object, by string) []map[string]any {
	if by == "role" {
		return byRole(Timing(evs, itemsChanged, "task"))
	}
	type hostTask struct{ host, task string }
	changedItem := map[hostTask]bool{}
	for _, o := range itemsChanged {
		changedItem[hostTask{asString(o["host_name"]), taskKey(o)}] = true
	}
	groups := map[string]*timing{}
	playRun := map[string]map[string]int{}
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
			// A play that runs in serial batches runs each task once per batch, under
			// the same task ID: the batch (the play's own ID) is part of the key, so
			// that a row is one pass over its hosts and its wall clock holds no
			// other work.
			key, slowestName = asString(Lookup(o, "event_data.task_uuid"))+"\x00"+asString(Lookup(o, "event_data.play_uuid"))+"\x00"+play+"\x00"+task, host
		}
		g := groups[key]
		if g == nil {
			g = &timing{row: map[string]any{}, hosts: map[string]bool{}, tasks: map[string]bool{}, first: i}
			if by != "host" {
				// Which run of the play this is: 1 unless the play name occurs
				// again (serial batches, or two plays of one name).
				id := asString(Lookup(o, "event_data.play_uuid"))
				if playRun[play] == nil {
					playRun[play] = map[string]int{}
				}
				if playRun[play][id] == 0 {
					playRun[play][id] = len(playRun[play]) + 1
				}
				g.row["play_run"] = playRun[play][id]
			}
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
		g.add(o, secs, slowestName, changedItem[hostTask{host, taskKey(o)}])
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

// byRole rolls task rows up into one row per role, in the order the roles first
// ran; tasks outside any role are the row with an empty role. A role's wall
// clock is the sum of its tasks' wall clocks: its tasks need not run in one
// stretch (a role is used by several plays, or runs once per serial batch).
func byRole(tasks []map[string]any) []map[string]any {
	rows := map[string]map[string]any{}
	var order []string
	for _, t := range tasks {
		role := asString(t["role"])
		r := rows[role]
		if r == nil {
			r = map[string]any{"role": role, "tasks": 0, "wall_seconds": nil, "host_seconds": 0.0, "slowest_seconds": 0.0, "slowest_task": "",
				"changed": 0, "unchanged": 0, "failed": 0, "skipped": 0, "unreachable": 0}
			rows[role] = r
			order = append(order, role)
		}
		r["tasks"] = r["tasks"].(int) + 1
		if w, ok := t["wall_seconds"].(float64); ok {
			have, _ := r["wall_seconds"].(float64)
			r["wall_seconds"] = round3(have + w)
			if w > r["slowest_seconds"].(float64) || r["slowest_task"] == "" {
				r["slowest_seconds"], r["slowest_task"] = w, t["task"]
			}
		}
		r["host_seconds"] = round3(r["host_seconds"].(float64) + t["host_seconds"].(float64))
		for _, k := range []string{"changed", "unchanged", "failed", "skipped", "unreachable"} {
			r[k] = r[k].(int) + t[k].(int)
		}
	}
	out := make([]map[string]any, 0, len(order))
	for _, role := range order {
		out = append(out, rows[role])
	}
	return out
}

// TimingSummary says what a run's time was spent on, from its task rows: every
// task counts with its wall clock under exactly one heading.
//
//	changed    a host changed something
//	failed     no host changed anything and one failed or was unreachable
//	skipped    every host skipped it
//	unchanged  it ran and changed nothing anywhere (a no-op, for this run)
//
// "How much of this run was no-op" is unchanged + skipped over total.
func TimingSummary(tasks []map[string]any) map[string]any {
	secs := map[string]float64{"changed": 0, "failed": 0, "skipped": 0, "unchanged": 0}
	count := map[string]int{"changed": 0, "failed": 0, "skipped": 0, "unchanged": 0}
	total, known := 0.0, false
	for _, t := range tasks {
		class := "unchanged"
		switch {
		case t["changed"].(int) > 0:
			class = "changed"
		case t["failed"].(int)+t["unreachable"].(int) > 0:
			class = "failed"
		case t["unchanged"].(int) == 0 && t["skipped"].(int) > 0:
			class = "skipped"
		}
		count[class]++
		if w, ok := t["wall_seconds"].(float64); ok {
			secs[class] += w
			total += w
			known = true
		}
	}
	out := map[string]any{"tasks": len(tasks), "tasks_by_outcome": count}
	if known {
		for k, v := range secs {
			secs[k] = round3(v)
		}
		out["task_seconds"] = round3(total)
		out["seconds_by_outcome"] = secs
	}
	return out
}

// RecapCheck compares a run's task results with the run's own count per host
// (what the PLAY RECAP prints), host by host: when they agree, no task result
// is missing from what a timing view was built on.
//
// Ansible's recap does not count the way the events read, so the results are
// counted its way first:
//
//   - "ok" includes "changed", a failure that was ignored, and one for every
//     file a host included (read from the include events' output line, which
//     names the hosts; where an include also left a task result, as
//     include_role does, it is counted once);
//   - a skipped meta task (meta: end_host under a condition) is reported as
//     skipped but not counted;
//   - a looped task whose item changed something counts as changed even when
//     the task's own result does not say so, because it ended failed or was
//     ignored (itemsChanged are those item results).
//
// A host with rescued failures is left out as unverifiable: how a rescue is
// counted cannot be read from the events with certainty.
func RecapCheck(results, includes, itemsChanged, hostSummaries []Object) map[string]any {
	type tally struct{ ok, changed, failed, skipped, unreachable int }
	got := map[string]*tally{}
	at := func(host string) *tally {
		if got[host] == nil {
			got[host] = &tally{}
		}
		return got[host]
	}
	type hostTask struct{ host, task string }
	saidChanged := map[hostTask]bool{}
	includeResult := map[hostTask]bool{}
	for _, o := range results {
		host, _ := o["host_name"].(string)
		t := at(host)
		switch o["event"] {
		case "runner_on_failed":
			if Lookup(o, "event_data.ignore_errors") == true {
				t.ok++
			} else {
				t.failed++
			}
		case "runner_on_unreachable":
			t.unreachable++
		case "runner_on_skipped":
			if action := asString(Lookup(o, "event_data.task_action")); action != "meta" && action != "ansible.builtin.meta" {
				t.skipped++
			}
		default:
			t.ok++
			if o["changed"] == true {
				t.changed++
				saidChanged[hostTask{host, taskKey(o)}] = true
			}
			if strings.Contains(asString(Lookup(o, "event_data.task_action")), "include") {
				includeResult[hostTask{host, taskKey(o)}] = true
			}
		}
	}
	counted := map[hostTask]bool{}
	for _, o := range itemsChanged {
		k := hostTask{asString(o["host_name"]), taskKey(o)}
		if !saidChanged[k] && !counted[k] {
			counted[k] = true
			at(k.host).changed++
		}
	}
	for _, o := range includes {
		for _, host := range includedHosts(asString(o["stdout"])) {
			// Some includes (include_role) also leave a task result, which is
			// counted already: the include stands in its place, once.
			if k := (hostTask{host, taskKey(o)}); includeResult[k] {
				includeResult[k] = false
				continue
			}
			at(host).ok++
		}
	}
	matching, unverifiable := 0, 0
	var differ []map[string]any
	for _, h := range hostSummaries {
		host := asString(h["host_name"])
		if asInt(h["rescued"]) > 0 {
			unverifiable++
			continue
		}
		t := at(host)
		diff := map[string]any{}
		for _, c := range []struct {
			name       string
			got, recap int
		}{{"ok", t.ok, asInt(h["ok"])}, {"changed", t.changed, asInt(h["changed"])}, {"failed", t.failed, asInt(h["failures"])},
			{"skipped", t.skipped, asInt(h["skipped"])}, {"unreachable", t.unreachable, asInt(h["dark"])}} {
			if c.got != c.recap {
				diff[c.name] = map[string]int{"task_results": c.got, "recap": c.recap}
			}
		}
		if len(diff) == 0 {
			matching++
			continue
		}
		diff["host"] = host
		differ = append(differ, diff)
	}
	out := map[string]any{"hosts": len(hostSummaries), "hosts_matching": matching, "matches": len(differ) == 0}
	if unverifiable > 0 {
		out["hosts_unverifiable"] = unverifiable // rescued failures
	}
	if len(differ) > 0 {
		out["hosts_differing"] = len(differ)
		if len(differ) > 5 {
			differ = differ[:5]
		}
		out["first_differences"] = differ
	}
	return out
}

// taskKey names the task of an event: its ID, or its play and name where an
// event carries no ID.
func taskKey(o Object) string {
	if id := asString(Lookup(o, "event_data.task_uuid")); id != "" {
		return id
	}
	return asString(o["play"]) + "\x00" + asString(o["task"])
}

// includedHosts reads the hosts out of an include's output line(s):
// "included: /path/tasks.yml for web1, web2" (a looped include adds
// " => (item=...)" after a host).
func includedHosts(stdout string) []string {
	var hosts []string
	for _, line := range strings.Split(StripANSI(stdout), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "included: ") {
			continue
		}
		i := strings.Index(line, " for ")
		if i < 0 {
			continue
		}
		for _, part := range strings.Split(line[i+5:], ", ") {
			if name := strings.Fields(part); len(name) > 0 {
				hosts = append(hosts, name[0])
			}
		}
	}
	return hosts
}

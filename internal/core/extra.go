package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---- tokens ----

// PlanTokenCreate builds the creation of an API token, for the caller or (as an
// administrator) for another user. The token's value is only in the step's Raw
// answer, never in the plan or the audit record.
func (c *Client) PlanTokenCreate(ctx context.Context, user, description, scope string) (*Plan, error) {
	if scope != "read" && scope != "write" {
		return nil, usagef("--scope is read or write")
	}
	if strings.TrimSpace(description) == "" {
		return nil, usagef("a token needs --description: say what it is for")
	}
	path, target := "/tokens/", "new token for the current user"
	if user != "" {
		u, err := c.Resolve(ctx, FindResource("users"), user, nil)
		if err != nil {
			return nil, err
		}
		path, target = "/users/"+idOf(u)+"/tokens/", fmt.Sprintf("new token for user %q (id %d)", u["username"], ID(u))
	}
	body := map[string]any{"description": description, "scope": scope}
	step := &Step{Target: target, Action: "create token", Method: http.MethodPost, Path: path, Send: body, body: body, Status: "planned",
		result: func(res Object) map[string]any {
			return map[string]any{"id": ID(res), "scope": res["scope"], "expires": res["expires"]}
		},
		verify: func(_ context.Context, _ *Client, res Object) error {
			if s, _ := res["token"].(string); s == "" {
				return errors.New("the answer holds no token")
			}
			return nil
		}}
	return &Plan{Command: "token create", Tier: "write", Steps: []*Step{step}}, nil
}

// ---- role grants ----

// roleModels maps a kind to the model name roles are defined on.
var roleModels = map[string]string{
	"inventories": "inventory", "job-templates": "jobtemplate", "workflow-templates": "workflowjobtemplate", "projects": "project",
	"credentials": "credential", "execution-environments": "executionenvironment", "instance-groups": "instancegroup",
	"notification-templates": "notificationtemplate", "organizations": "organization", "teams": "team",
}

// RoleKinds lists the kinds roles can be granted on.
func RoleKinds() []string {
	out := make([]string, 0, len(roleModels))
	for k := range roleModels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PlanGrant builds giving a role to a user or a team, on one object or (for a
// system-wide role) on none; revoke takes it away. On AAP the grant is made on
// the platform gateway, where roles, users and teams live.
func (c *Client) PlanGrant(ctx context.Context, role, user, team string, target *Resource, targetRef string, scope url.Values, revoke bool) (*Plan, error) {
	if (user == "") == (team == "") {
		return nil, usagef("give --user or --team (one of them)")
	}
	rd, err := c.Resolve(ctx, FindResource("role-definitions"), role, nil)
	if err != nil {
		return nil, err
	}
	ct, _ := rd["content_type"].(string)
	body := map[string]any{"role_definition": ID(rd)}
	q := url.Values{"role_definition": {idOf(rd)}}
	on := "system-wide"
	switch {
	case target == nil && ct != "":
		return nil, usagef("the role %q applies to one %s: name it with --on KIND=NAME", rd["name"], ct[strings.LastIndex(ct, ".")+1:])
	case target != nil:
		model, ok := roleModels[target.Kind]
		if !ok {
			return nil, usagef("roles cannot be granted on %s; possible: %s", target.Kind, strings.Join(RoleKinds(), ", "))
		}
		if !strings.HasSuffix(ct, "."+model) {
			return nil, usagef("the role %q applies to %q, not to a %s", rd["name"], ct, target.Singular)
		}
		obj, err := c.Resolve(ctx, target, targetRef, scope)
		if err != nil {
			return nil, err
		}
		body["object_id"] = ID(obj)
		q.Set("object_id", idOf(obj))
		on = "on " + label(target, obj)
	}
	path, who := "/role_user_assignments/", ""
	if user != "" {
		u, err := c.Resolve(ctx, FindResource("users"), user, nil)
		if err != nil {
			return nil, err
		}
		body["user"], who = ID(u), fmt.Sprintf("user %q", u["username"])
		q.Set("user", idOf(u))
	} else {
		t, err := c.Resolve(ctx, FindResource("teams"), team, nil)
		if err != nil {
			return nil, err
		}
		path, who = "/role_team_assignments/", fmt.Sprintf("team %q", t["name"])
		body["team"] = ID(t)
		q.Set("team", idOf(t))
	}
	existing, _, err := c.List(ctx, path, q, 5)
	if err != nil {
		return nil, err
	}
	if target == nil { // a system-wide grant has no object: leave out those that have one
		var kept []Object
		for _, e := range existing {
			if e["object_id"] == nil || e["object_id"] == "" {
				kept = append(kept, e)
			}
		}
		existing = kept
	}
	verb := "grant"
	if revoke {
		verb = "revoke"
	}
	p := &Plan{Command: verb, Tier: "write"}
	name := fmt.Sprintf("role %q for %s %s", rd["name"], who, on)
	switch {
	case !revoke && len(existing) == 0:
		p.Steps = append(p.Steps, &Step{Target: name, Action: "grant", Method: http.MethodPost, Path: path, Send: body, body: body, Status: "planned",
			result: func(res Object) map[string]any { return map[string]any{"assignment_id": ID(res)} }})
	case revoke:
		for _, e := range existing {
			p.Steps = append(p.Steps, &Step{Target: name, Action: "revoke", Method: http.MethodDelete, Path: path + idOf(e) + "/",
				Before: map[string]any{"assignment_id": ID(e), "created": e["created"]}, Status: "planned"})
		}
	}
	return p, nil
}

// AccessFields are the flat fields of one entry of an object's access list.
var AccessFields = []Field{f("principal", "WHO"), f("type", "TYPE"), f("role", "ROLE"), f("assignment_id", "ASSIGNMENT"), w("created")}

// Access lists who has which role on one object: the direct grants to users and
// teams (not what is inherited from an organization role or from being a
// superuser).
func (c *Client) Access(ctx context.Context, target *Resource, ref string, scope url.Values) ([]map[string]any, error) {
	model, ok := roleModels[target.Kind]
	if !ok {
		return nil, usagef("roles do not apply to %s; possible: %s", target.Kind, strings.Join(RoleKinds(), ", "))
	}
	obj, err := c.Resolve(ctx, target, ref, scope)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, kind := range []struct{ path, typ, name string }{
		{"/role_user_assignments/", "user", "summary_fields.user.username"},
		{"/role_team_assignments/", "team", "summary_fields.team.name"},
	} {
		list, _, err := c.List(ctx, kind.path, url.Values{"object_id": {idOf(obj)}}, 0)
		if err != nil {
			return nil, err
		}
		for _, a := range list {
			if ct, _ := a["content_type"].(string); !strings.HasSuffix(ct, "."+model) {
				continue // the same number on another kind of object
			}
			rows = append(rows, map[string]any{"principal": Lookup(a, kind.name), "type": kind.typ,
				"role": Lookup(a, "summary_fields.role_definition.name"), "assignment_id": ID(a), "created": a["created"]})
		}
	}
	return rows, nil
}

// ---- hosts and inventories: facts, variables ----

// MergedVars computes the variables a host ends up with: inventory variables,
// then its groups from the outermost in, then the host's own. sources says where
// each variable's final value came from. Values of secret-looking names are
// masked unless showSecrets.
func (c *Client) MergedVars(ctx context.Context, host Object, showSecrets bool) (vars map[string]any, sources map[string]string, err error) {
	name, _ := host["name"].(string)
	var script map[string]any
	if err := c.GetAny(ctx, "/inventories/"+strconv.Itoa(asInt(host["inventory"]))+"/script/?hostvars=1&all=1", &script); err != nil {
		return nil, nil, err
	}
	group := func(g string) map[string]any { m, _ := script[g].(map[string]any); return m }
	list := func(v any) []string {
		var out []string
		if l, ok := v.([]any); ok {
			for _, it := range l {
				if s, ok := it.(string); ok {
					out = append(out, s)
				}
			}
		}
		return out
	}
	// depth of every group below "all", and which groups hold the host
	depth := map[string]int{"all": 0}
	parents := map[string][]string{}
	queue := []string{"all"}
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		for _, child := range list(group(g)["children"]) {
			parents[child] = append(parents[child], g)
			if d, seen := depth[child]; !seen || d < depth[g]+1 {
				depth[child] = depth[g] + 1
				if len(depth) < 100000 {
					queue = append(queue, child)
				}
			}
		}
	}
	member := map[string]bool{"all": true}
	var mark func(g string, hops int)
	mark = func(g string, hops int) {
		if member[g] && g != "all" || hops > 64 {
			return
		}
		member[g] = true
		for _, p := range parents[g] {
			mark(p, hops+1)
		}
	}
	for g := range script {
		if g == "_meta" || g == "all" {
			continue
		}
		for _, h := range list(group(g)["hosts"]) {
			if h == name {
				mark(g, 0)
			}
		}
	}
	order := make([]string, 0, len(member))
	for g := range member {
		order = append(order, g)
	}
	sort.Slice(order, func(i, j int) bool {
		if depth[order[i]] != depth[order[j]] {
			return depth[order[i]] < depth[order[j]]
		}
		return order[i] < order[j]
	})
	vars, sources = map[string]any{}, map[string]string{}
	for _, g := range order {
		gv, _ := group(g)["vars"].(map[string]any)
		for k, v := range gv {
			vars[k] = v
			sources[k] = "group " + g
			if g == "all" {
				sources[k] = "inventory"
			}
		}
	}
	if hv, ok := Lookup(script, "_meta.hostvars").(map[string]any); ok {
		if own, ok := hv[name].(map[string]any); ok {
			for k, v := range own {
				if k == "remote_tower_enabled" || k == "remote_tower_id" || k == "remote_host_enabled" || k == "remote_host_id" {
					continue // added by the controller, not a variable of the host
				}
				vars[k], sources[k] = v, "host"
			}
		}
	}
	if !showSecrets {
		vars = maskMap(vars)
	}
	return vars, sources, nil
}

// ---- schedules ----

// SchedulePreview asks the controller for the next runs of a recurrence rule. It
// is a POST that changes nothing.
func (c *Client) SchedulePreview(ctx context.Context, rrule string) (utc, local []string, err error) {
	if err := c.ready(ctx); err != nil {
		return nil, nil, err
	}
	var out struct {
		UTC   []string `json:"utc"`
		Local []string `json:"local"`
	}
	err = c.do(ctx, http.MethodPost, c.apiPath+"/schedules/preview/", map[string]any{"rrule": rrule}, &out)
	var unknown *OutcomeUnknownError
	if errors.As(err, &unknown) { // nothing was changed: it is a plain failure
		err = unknown.Cause
	}
	return out.UTC, out.Local, err
}

// ---- copy ----

var copyable = map[string]bool{"job-templates": true, "workflow-templates": true, "inventories": true, "projects": true, "credentials": true,
	"notification-templates": true, "execution-environments": true}

// PlanCopy builds copying an object under a new name. A copied inventory gets
// its hosts and groups in the background, shortly after.
func (c *Client) PlanCopy(ctx context.Context, r *Resource, ref, newName string, scope url.Values) (*Plan, error) {
	if !copyable[r.Kind] {
		var kinds []string
		for k := range copyable {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		return nil, usagef("%s cannot be copied; possible: %s", r.Kind, strings.Join(kinds, ", "))
	}
	if strings.TrimSpace(newName) == "" {
		return nil, usagef("give the new name with --name")
	}
	o, err := c.Resolve(ctx, r, ref, scope)
	if err != nil {
		return nil, err
	}
	if _, err := c.Resolve(ctx, r, "name:"+newName, nil); err == nil {
		return nil, usagef("a %s named %q already exists", r.Singular, newName)
	} else if !errors.Is(err, ErrNotFound) {
		var amb *AmbiguousError
		if !errors.As(err, &amb) {
			return nil, err
		}
		return nil, usagef("a %s named %q already exists", r.Singular, newName)
	}
	path := r.Path + idOf(o) + "/copy/"
	info, err := c.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if info["can_copy"] != true {
		return nil, usagef("%s cannot be copied by this user (the controller says can_copy is false)", label(r, o))
	}
	body := map[string]any{"name": newName}
	step := &Step{Target: label(r, o) + " -> " + strconv.Quote(newName), Action: "copy", Method: http.MethodPost, Path: path, Send: body, body: body, Status: "planned",
		result: func(res Object) map[string]any { return map[string]any{"id": ID(res), "name": res["name"]} }}
	if r.Kind == "inventories" {
		step.Impact = "hosts and groups are copied in the background"
	}
	return &Plan{Command: "copy " + r.Singular, Tier: "write", Steps: []*Step{step}}, nil
}

// ---- surveys ----

// Survey reads a template's survey: whether it is on, and its questions in the
// short form `survey set` takes.
func (c *Client) Survey(ctx context.Context, r *Resource, tmpl Object) (map[string]any, error) {
	spec, err := c.Get(ctx, r.Path+idOf(tmpl)+"/survey_spec/")
	if err != nil {
		return nil, err
	}
	questions := []any{}
	if list, ok := spec["spec"].([]any); ok {
		for _, q := range list {
			m, ok := q.(map[string]any)
			if !ok {
				continue
			}
			item := map[string]any{"variable": m["variable"], "question": m["question_name"], "type": m["type"], "required": m["required"]}
			if d, _ := m["question_description"].(string); d != "" {
				item["description"] = d
			}
			if m["default"] != nil && m["default"] != "" {
				item["default"] = m["default"]
				if m["type"] == "password" {
					item["default"] = "******"
				}
			}
			switch ch := m["choices"].(type) {
			case string:
				if ch != "" {
					item["choices"] = strings.Split(ch, "\n")
				}
			case []any:
				if len(ch) > 0 {
					item["choices"] = ch
				}
			}
			for _, k := range []string{"min", "max"} {
				if m[k] != nil {
					item[k] = m[k]
				}
			}
			questions = append(questions, item)
		}
	}
	return map[string]any{"template": tmpl["name"], "id": ID(tmpl), "enabled": tmpl["survey_enabled"], "name": spec["name"], "description": spec["description"], "questions": questions}, nil
}

var surveyTypes = map[string]bool{"text": true, "textarea": true, "password": true, "integer": true, "float": true, "multiplechoice": true, "multiselect": true}

// PlanSurveySet builds replacing a template's survey with the given questions
// (the short form: variable, question, type, required, default, choices, min,
// max, description) and switching it on. No questions removes the survey.
func (c *Client) PlanSurveySet(ctx context.Context, r *Resource, ref string, questions []map[string]any, remove bool) (*Plan, error) {
	tmpl, err := c.Resolve(ctx, r, ref, nil)
	if err != nil {
		return nil, err
	}
	base := r.Path + idOf(tmpl) + "/"
	old, err := c.Survey(ctx, r, tmpl)
	if err != nil {
		return nil, err
	}
	before := map[string]any{"enabled": old["enabled"], "questions": old["questions"]}
	p := &Plan{Command: "survey set", Tier: "write"}
	if remove {
		p.Command = "survey remove"
		if len(old["questions"].([]any)) == 0 && tmpl["survey_enabled"] != true {
			return p, nil
		}
		p.Steps = append(p.Steps, &Step{Target: label(r, tmpl), Action: "remove survey", Method: http.MethodDelete, Path: base + "survey_spec/", Before: before, Status: "planned"})
		if tmpl["survey_enabled"] == true {
			off := map[string]any{"survey_enabled": false}
			p.Steps = append(p.Steps, &Step{Target: label(r, tmpl), Action: "switch survey off", Method: http.MethodPatch, Path: base, Send: off, body: off, Status: "planned"})
		}
		return p, nil
	}
	if len(questions) == 0 {
		return nil, usagef("the file holds no questions: use `survey remove` to remove a survey")
	}
	seen := map[string]bool{}
	spec := make([]any, 0, len(questions))
	for i, q := range questions {
		variable, _ := q["variable"].(string)
		if variable == "" || strings.ContainsAny(variable, " \t") {
			return nil, usagef("question %d: variable is required and has no spaces", i+1)
		}
		if seen[variable] {
			return nil, usagef("question %d: the variable %q is used twice", i+1, variable)
		}
		seen[variable] = true
		typ, _ := q["type"].(string)
		if typ == "" {
			typ = "text"
		}
		if !surveyTypes[typ] {
			return nil, usagef("question %d (%s): unknown type %q; types: text, textarea, password, integer, float, multiplechoice, multiselect", i+1, variable, typ)
		}
		name, _ := q["question"].(string)
		if name == "" {
			name = variable
		}
		item := map[string]any{"variable": variable, "question_name": name, "type": typ, "required": q["required"] != false, "question_description": "", "default": "", "choices": "", "min": nil, "max": nil}
		if d, ok := q["description"].(string); ok {
			item["question_description"] = d
		}
		if q["default"] != nil {
			if q["default"] == "******" {
				return nil, usagef("question %d (%s): the default is the mask ******: give the real default or leave it out", i+1, variable)
			}
			item["default"] = q["default"]
		}
		for _, k := range []string{"min", "max"} {
			if q[k] != nil {
				item[k] = q[k]
			}
		}
		if ch, ok := q["choices"].([]any); ok {
			item["choices"] = ch
		}
		if (typ == "multiplechoice" || typ == "multiselect") && item["choices"] == "" {
			return nil, usagef("question %d (%s): a %s question needs choices", i+1, variable, typ)
		}
		for k := range q {
			switch k {
			case "variable", "question", "type", "required", "default", "choices", "min", "max", "description":
			default:
				return nil, usagef("question %d (%s): unknown key %q", i+1, variable, k)
			}
		}
		spec = append(spec, item)
	}
	body := map[string]any{"name": "", "description": "", "spec": spec}
	p.Steps = append(p.Steps, &Step{Target: label(r, tmpl), Action: "set survey", Method: http.MethodPost, Path: base + "survey_spec/", Send: maskSurvey(body), body: body, Before: before, Status: "planned"})
	if tmpl["survey_enabled"] != true {
		on := map[string]any{"survey_enabled": true}
		p.Steps = append(p.Steps, &Step{Target: label(r, tmpl), Action: "switch survey on", Method: http.MethodPatch, Path: base, Send: on, body: on, Status: "planned"})
	}
	return p, nil
}

// maskSurvey hides the defaults of password questions in what is shown and recorded.
func maskSurvey(body map[string]any) map[string]any {
	spec, _ := body["spec"].([]any)
	shown := make([]any, len(spec))
	for i, q := range spec {
		m := q.(map[string]any)
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		if cp["type"] == "password" && cp["default"] != "" {
			cp["default"] = "******"
		}
		shown[i] = cp
	}
	return map[string]any{"spec": shown}
}

// ---- notifications ----

var notifyParents = map[string]bool{"job-templates": true, "workflow-templates": true, "projects": true, "inventory-sources": true}

// PlanNotify builds attaching a notification template to a template, project or
// inventory source for one event (started, success or error), or detaching it.
func (c *Client) PlanNotify(ctx context.Context, parent *Resource, parentRef, notification, when string, scope url.Values, detach bool) (*Plan, error) {
	if !notifyParents[parent.Kind] {
		return nil, usagef("notifications attach to job-templates, workflow-templates, projects or inventory-sources")
	}
	if when != "started" && when != "success" && when != "error" {
		return nil, usagef("--on is started, success or error")
	}
	po, err := c.Resolve(ctx, parent, parentRef, scope)
	if err != nil {
		return nil, err
	}
	nt := FindResource("notification-templates")
	no, err := c.Resolve(ctx, nt, notification, nil)
	if err != nil {
		return nil, err
	}
	path := parent.Path + idOf(po) + "/notification_templates_" + when + "/"
	_, n, err := c.List(ctx, path, url.Values{"id": {idOf(no)}}, 1)
	if err != nil {
		return nil, err
	}
	verb := "attach"
	if detach {
		verb = "detach"
	}
	p := &Plan{Command: verb + " notification", Tier: "write"}
	if (n > 0) != detach {
		return p, nil
	}
	body := map[string]any{"id": ID(no)}
	if detach {
		body["disassociate"] = true
	}
	p.Steps = append(p.Steps, &Step{Target: fmt.Sprintf("%s on %s, when %s", label(nt, no), label(parent, po), when), Action: verb,
		Method: http.MethodPost, Path: path, Send: body, body: body, Before: map[string]any{"attached": n > 0}, Status: "planned"})
	return p, nil
}

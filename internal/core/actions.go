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

// label is how a step names its target.
func label(r *Resource, o Object) string {
	name := ""
	if r.NameField != "" {
		name, _ = o[r.NameField].(string)
	} else {
		name, _ = o["name"].(string)
	}
	if name == "" {
		return fmt.Sprintf("%s id %d", r.Singular, ID(o))
	}
	return fmt.Sprintf("%s %q (id %d)", r.Singular, name, ID(o))
}

// LaunchOptions are the prompts a launch may answer. An empty value means "use
// the template's own".
type LaunchOptions struct {
	Limit     string
	Tags      string
	SkipTags  string
	ExtraVars map[string]any
	Inventory string // name or id
	Check     bool   // Ansible check mode
	Diff      bool
	Branch    string
	Verbosity int // -1 = not given
}

// launchPrompts maps a request field to the template setting that must allow it.
var launchPrompts = map[string]string{
	"limit": "ask_limit_on_launch", "job_tags": "ask_tags_on_launch", "skip_tags": "ask_skip_tags_on_launch",
	"extra_vars": "ask_variables_on_launch", "inventory": "ask_inventory_on_launch", "job_type": "ask_job_type_on_launch",
	"diff_mode": "ask_diff_mode_on_launch", "scm_branch": "ask_scm_branch_on_launch", "verbosity": "ask_verbosity_on_launch",
}

// LaunchInfo reads what a template asks for at launch: its prompts, defaults and
// survey questions (password defaults masked).
func (c *Client) LaunchInfo(ctx context.Context, r *Resource, tmpl Object) (map[string]any, error) {
	base := r.Path + idOf(tmpl) + "/"
	info, err := c.Get(ctx, base+"launch/")
	if err != nil {
		return nil, err
	}
	out := map[string]any{"id": ID(tmpl), "name": tmpl["name"], "type": r.Singular}
	prompts := []string{}
	for k, v := range info {
		if strings.HasPrefix(k, "ask_") && v == true {
			prompts = append(prompts, strings.TrimSuffix(strings.TrimPrefix(k, "ask_"), "_on_launch"))
		}
	}
	sort.Strings(prompts)
	out["prompts"] = prompts
	for _, k := range []string{"can_start_without_user_input", "variables_needed_to_start", "passwords_needed_to_start", "survey_enabled", "credential_needed_to_start", "inventory_needed_to_start", "defaults"} {
		if v, ok := info[k]; ok {
			out[k] = Mask(v)
		}
	}
	questions := []any{}
	if info["survey_enabled"] == true {
		spec, err := c.Get(ctx, base+"survey_spec/")
		if err != nil {
			return nil, err
		}
		if list, ok := spec["spec"].([]any); ok {
			for _, q := range list {
				m, ok := q.(map[string]any)
				if !ok {
					continue
				}
				item := map[string]any{}
				for _, k := range []string{"variable", "question_name", "question_description", "type", "required", "choices", "min", "max", "default"} {
					if v, ok := m[k]; ok && v != "" && v != nil {
						item[k] = v
					}
				}
				if m["type"] == "password" && item["default"] != nil {
					item["default"] = "******"
				}
				questions = append(questions, item)
			}
		}
	}
	out["survey"] = questions
	return out, nil
}

// PlanLaunch builds the launch of a job template or workflow template. It refuses
// what the controller would silently ignore: a --limit the template does not
// prompt for would otherwise run against every host.
func (c *Client) PlanLaunch(ctx context.Context, r *Resource, ref string, o LaunchOptions) (*Plan, error) {
	tmpl, err := c.Resolve(ctx, r, ref, nil)
	if err != nil {
		return nil, err
	}
	base := r.Path + idOf(tmpl) + "/launch/"
	info, err := c.Get(ctx, base)
	if err != nil {
		return nil, err
	}
	body := map[string]any{}
	if o.Limit != "" {
		body["limit"] = o.Limit
	}
	if o.Tags != "" {
		body["job_tags"] = o.Tags
	}
	if o.SkipTags != "" {
		body["skip_tags"] = o.SkipTags
	}
	if len(o.ExtraVars) > 0 {
		body["extra_vars"] = o.ExtraVars
	}
	if o.Check {
		body["job_type"] = "check"
	}
	if o.Diff {
		body["diff_mode"] = true
	}
	if o.Branch != "" {
		body["scm_branch"] = o.Branch
	}
	if o.Verbosity >= 0 {
		body["verbosity"] = o.Verbosity
	}
	if o.Inventory != "" {
		inv, err := c.Resolve(ctx, FindResource("inventories"), o.Inventory, nil)
		if err != nil {
			return nil, err
		}
		body["inventory"] = ID(inv)
	}
	var refused []string
	for field := range body {
		ask := launchPrompts[field]
		if info[ask] == true {
			continue
		}
		// Survey answers travel in extra_vars and are accepted without the prompt.
		if field == "extra_vars" && info["survey_enabled"] == true {
			continue
		}
		refused = append(refused, fmt.Sprintf("%s (the template does not have %s)", field, ask))
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return nil, usagef("the controller would ignore %s and launch without it; enable the prompt on the template or drop the option", strings.Join(refused, ", "))
	}
	var missing []string
	if vars, ok := info["variables_needed_to_start"].([]any); ok {
		for _, v := range vars {
			if name, _ := v.(string); name != "" {
				if _, given := o.ExtraVars[name]; !given {
					missing = append(missing, name)
				}
			}
		}
	}
	if len(missing) > 0 {
		return nil, usagef("the survey requires these variables: %s (give them with --extra-vars)", strings.Join(missing, ", "))
	}
	if pw, ok := info["passwords_needed_to_start"].([]any); ok && len(pw) > 0 {
		return nil, usagef("the template asks for passwords at launch (%v), which awxctl does not send", pw)
	}
	if info["inventory_needed_to_start"] == true && body["inventory"] == nil {
		return nil, usagef("the template has no inventory: give --inventory")
	}
	if info["credential_needed_to_start"] == true {
		return nil, usagef("the template has no credential and asks for one at launch, which awxctl does not send")
	}
	step := &Step{
		Target: label(r, tmpl), Action: "launch", Method: http.MethodPost, Path: base, Send: maskMap(body), body: body, Status: "planned",
		Before: map[string]any{"defaults": Mask(info["defaults"])},
		result: func(res Object) map[string]any {
			out := map[string]any{"job_id": ID(res), "status": res["status"], "type": res["type"]}
			if ign, ok := res["ignored_fields"].(map[string]any); ok && len(ign) > 0 {
				out["ignored_fields"] = Mask(ign)
			}
			return out
		},
		verify: func(_ context.Context, _ *Client, res Object) error {
			if ID(res) == 0 {
				return errors.New("the answer names no job")
			}
			return nil
		},
	}
	return &Plan{Command: "launch " + r.Singular, Tier: "operator", Steps: []*Step{step}}, nil
}

// PlanRelaunch builds the relaunch of a job; hosts is "", "all" or "failed".
func (c *Client) PlanRelaunch(ctx context.Context, id int, hosts string) (*Plan, error) {
	j, err := c.FindJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.Type != "job" && j.Type != "workflow_job" && j.Type != "ad_hoc_command" {
		return nil, usagef("a %s cannot be relaunched: start a new sync instead", strings.ReplaceAll(j.Type, "_", " "))
	}
	body := map[string]any{}
	if hosts != "" && hosts != "all" {
		if j.Type != "job" {
			return nil, usagef("--hosts applies to playbook jobs only")
		}
		if hosts != "failed" {
			return nil, usagef("--hosts takes all or failed")
		}
		body["hosts"] = "failed"
	}
	info, err := c.Get(ctx, j.Path+"relaunch/")
	if err != nil {
		return nil, err
	}
	if pw, ok := info["passwords_needed_to_start"].([]any); ok && len(pw) > 0 {
		return nil, usagef("the job asks for passwords at launch (%v), which awxctl does not send", pw)
	}
	before := map[string]any{"status": j.Status(), "limit": j.Obj["limit"], "job_type": j.Obj["job_type"]}
	if body["hosts"] == "failed" {
		before["failed_hosts"] = info["retry_counts"]
	}
	step := &Step{
		Target: fmt.Sprintf("%s %d %q", strings.ReplaceAll(j.Type, "_", " "), j.ID, j.Obj["name"]), Action: "relaunch",
		Method: http.MethodPost, Path: j.Path + "relaunch/", Send: body, body: body, Before: before, Status: "planned",
		result: func(res Object) map[string]any { return map[string]any{"job_id": ID(res), "status": res["status"]} },
		verify: func(_ context.Context, _ *Client, res Object) error {
			if ID(res) == 0 {
				return errors.New("the answer names no job")
			}
			return nil
		},
	}
	return &Plan{Command: "relaunch", Tier: "operator", Steps: []*Step{step}}, nil
}

// PlanCancel builds the cancel of running jobs. A job that cannot be canceled
// (already finished) is an error before anything is sent.
func (c *Client) PlanCancel(ctx context.Context, ids []int) (*Plan, error) {
	p := &Plan{Command: "cancel", Tier: "operator"}
	for _, id := range ids {
		j, err := c.FindJob(ctx, id)
		if err != nil {
			return nil, err
		}
		info, err := c.Get(ctx, j.Path+"cancel/")
		if err != nil {
			return nil, err
		}
		if info["can_cancel"] != true {
			return nil, usagef("job %d cannot be canceled (status %s)", id, j.Status())
		}
		p.Steps = append(p.Steps, &Step{
			Target: fmt.Sprintf("%s %d %q", strings.ReplaceAll(j.Type, "_", " "), j.ID, j.Obj["name"]), Action: "cancel",
			Method: http.MethodPost, Path: j.Path + "cancel/", Before: map[string]any{"status": j.Status()}, Status: "planned",
		})
	}
	return p, nil
}

// PlanSync builds the sync of projects or inventory sources.
func (c *Client) PlanSync(ctx context.Context, r *Resource, refs []string, scope url.Values) (*Plan, error) {
	p := &Plan{Command: "sync " + r.Singular, Tier: "operator"}
	for _, ref := range refs {
		o, err := c.Resolve(ctx, r, ref, scope)
		if err != nil {
			return nil, err
		}
		path := r.Path + idOf(o) + "/update/"
		info, err := c.Get(ctx, path)
		if err != nil {
			return nil, err
		}
		if info["can_update"] != true {
			return nil, usagef("%s cannot be synced (a manual project, or a source without what it needs)", label(r, o))
		}
		p.Steps = append(p.Steps, &Step{
			Target: label(r, o), Action: "sync", Method: http.MethodPost, Path: path, Status: "planned",
			Before: map[string]any{"status": o["status"], "scm_revision": o["scm_revision"], "last_updated": o["last_updated"]},
			result: func(res Object) map[string]any { return map[string]any{"job_id": ID(res), "status": res["status"]} },
		})
	}
	return p, nil
}

// PlanApproval builds the approve or deny of pending workflow approvals.
func (c *Client) PlanApproval(ctx context.Context, ids []int, approve bool) (*Plan, error) {
	verb := "deny"
	if approve {
		verb = "approve"
	}
	r := FindResource("workflow-approvals")
	p := &Plan{Command: verb, Tier: "operator"}
	for _, id := range ids {
		o, err := c.Resolve(ctx, r, "id:"+strconv.Itoa(id), nil)
		if err != nil {
			return nil, err
		}
		if st, _ := o["status"].(string); st != "pending" {
			return nil, usagef("approval %d is not pending (status %s)", id, st)
		}
		p.Steps = append(p.Steps, &Step{
			Target: fmt.Sprintf("workflow approval %d %q (workflow job %v)", id, o["name"], Lookup(o, "summary_fields.workflow_job.id")), Action: verb,
			Method: http.MethodPost, Path: r.Path + idOf(o) + "/" + verb + "/", Before: map[string]any{"status": "pending"}, Status: "planned",
		})
	}
	return p, nil
}

// AdHocOptions describe one ad hoc command run.
type AdHocOptions struct {
	Inventory  string
	Credential string
	Limit      string
	Module     string
	Args       string
	Become     bool
	Check      bool
	Forks      int
}

// PlanAdHoc builds an ad hoc command: one module run against hosts of an
// inventory. It runs arbitrary commands, so it needs the write tier.
func (c *Client) PlanAdHoc(ctx context.Context, o AdHocOptions) (*Plan, error) {
	if o.Limit == "" {
		return nil, usagef("an ad hoc command needs --limit (use --limit all for every host, on purpose)")
	}
	inv, err := c.Resolve(ctx, FindResource("inventories"), o.Inventory, nil)
	if err != nil {
		return nil, err
	}
	cred, err := c.Resolve(ctx, FindResource("credentials"), o.Credential, nil)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"inventory": ID(inv), "credential": ID(cred), "limit": o.Limit, "module_name": o.Module, "module_args": o.Args, "become_enabled": o.Become}
	if o.Check {
		body["job_type"] = "check"
	}
	if o.Forks > 0 {
		body["forks"] = o.Forks
	}
	step := &Step{
		Target: fmt.Sprintf("inventory %q (id %d), limit %q", inv["name"], ID(inv), o.Limit), Action: "run " + o.Module,
		Method: http.MethodPost, Path: "/ad_hoc_commands/", Send: map[string]any{"module_name": o.Module, "module_args": o.Args, "limit": o.Limit, "credential": cred["name"], "become_enabled": o.Become, "job_type": body["job_type"]},
		body: body, Status: "planned", Before: map[string]any{"inventory_hosts": inv["total_hosts"]},
		result: func(res Object) map[string]any { return map[string]any{"job_id": ID(res), "status": res["status"]} },
	}
	return &Plan{Command: "run command", Tier: "write", Steps: []*Step{step}}, nil
}

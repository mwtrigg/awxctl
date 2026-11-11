package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// prepare checks that every field may be written and turns names into IDs for the
// fields that point at other objects.
func (c *Client) prepare(ctx context.Context, r *Resource, values map[string]any) (map[string]any, error) {
	if r.Writable == nil {
		return nil, usagef("%s cannot be changed with awxctl", r.Kind)
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		if !r.CanWrite(k) {
			fields := append([]string(nil), r.Writable...)
			sort.Strings(fields)
			return nil, usagef("%q is not a field of a %s that awxctl sets; the fields are: %s", k, r.Singular, strings.Join(fields, ", "))
		}
		if kind, isRef := r.Refs[k]; isRef && v != nil {
			if s, ok := v.(string); ok && s != "" {
				// a controller object points at the controller's own ID of the target
				target, err := c.Resolve(ControllerOnly(ctx), FindResource(kind), s, nil)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", k, err)
				}
				v = ID(target)
			} else if ok {
				v = nil // an empty value clears the reference
			}
		}
		out[k] = v
	}
	return out, nil
}

func scopeOf(r *Resource, values map[string]any) url.Values {
	q := url.Values{}
	if r.Scope != "" && values[r.Scope] != nil {
		q.Set(r.Scope, fmt.Sprint(values[r.Scope]))
	}
	return q
}

// PlanCreate builds the creation of one object.
func (c *Client) PlanCreate(ctx context.Context, r *Resource, values map[string]any) (*Plan, error) {
	body, err := c.prepare(ctx, r, values)
	if err != nil {
		return nil, err
	}
	name, _ := body[r.NameField].(string)
	if name == "" {
		return nil, usagef("a new %s needs %s", r.Singular, r.NameField)
	}
	q := scopeOf(r, body)
	q.Set(r.NameField, name)
	if org := body["organization"]; org != nil && r.Scope == "" {
		q.Set("organization", fmt.Sprint(org))
	}
	existing, _, err := c.List(ctx, r.Path, q, 1)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, usagef("%s already exists: use update", label(r, existing[0]))
	}
	step := &Step{
		Target: fmt.Sprintf("new %s %q", r.Singular, name), Action: "create", Method: http.MethodPost, Path: r.Path,
		Send: maskMap(body), body: body, Status: "planned",
		result: func(res Object) map[string]any { return map[string]any{"id": ID(res), r.NameField: res[r.NameField]} },
		verify: func(ctx context.Context, c *Client, res Object) error {
			if ID(res) == 0 {
				return errors.New("the answer names no new object")
			}
			_, err := c.Get(ctx, r.Path+idOf(res)+"/")
			return err
		},
	}
	return &Plan{Command: "create " + r.Singular, Tier: "write", Steps: []*Step{step}}, nil
}

// same compares a current API value with a wanted one, by their JSON form. A
// number and a text that read the same are the same: the controller stores 42
// in a text field as "42", and that is the change that was asked for.
func same(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) == string(jb) {
		return true
	}
	return isScalar(a) && isScalar(b) && fmt.Sprint(a) == fmt.Sprint(b)
}

func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, int, int64, float64, json.Number:
		return true
	}
	return false
}

// PlanUpdate builds the change of the given fields on each object. A field that
// already has the wanted value is left out; an object with nothing to change gets
// no step.
func (c *Client) PlanUpdate(ctx context.Context, r *Resource, refs []string, scope url.Values, values map[string]any) (*Plan, error) {
	body, err := c.prepare(ctx, r, values)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, usagef("nothing to change: give at least one field")
	}
	p := &Plan{Command: "update " + r.Singular, Tier: "write"}
	for _, ref := range refs {
		o, err := c.Resolve(ctx, r, ref, scope)
		if err != nil {
			return nil, err
		}
		change, before := map[string]any{}, map[string]any{}
		for k, v := range body {
			// A secret reads back as $encrypted$, so it can never be compared: send it.
			if !secretKey.MatchString(k) && same(o[k], v) {
				continue
			}
			change[k], before[k] = v, o[k]
		}
		if len(change) == 0 {
			continue
		}
		p.Steps = append(p.Steps, &Step{
			Target: label(r, o), Action: "update", Method: http.MethodPatch, Path: r.Path + idOf(o) + "/",
			Send: maskMap(change), body: change, Before: maskMap(before), Status: "planned",
			verify: func(_ context.Context, _ *Client, res Object) error {
				for k, v := range change {
					if !secretKey.MatchString(k) && !same(res[k], v) {
						return fmt.Errorf("%s is %v after the update, not %v", k, res[k], v)
					}
				}
				return nil
			},
		})
	}
	return p, nil
}

// PlanDelete builds the deletion of objects, with their before-state and what
// goes with them.
func (c *Client) PlanDelete(ctx context.Context, r *Resource, refs []string, scope url.Values) (*Plan, error) {
	if !r.Deletable() {
		return nil, usagef("%s cannot be deleted with awxctl", r.Kind)
	}
	p := &Plan{Command: "delete " + r.Singular, Tier: "write"}
	seen := map[int]bool{}
	for _, ref := range refs {
		o, err := c.Resolve(ctx, r, ref, scope)
		if err != nil {
			return nil, err
		}
		if seen[ID(o)] {
			continue
		}
		seen[ID(o)] = true
		if st, _ := o["status"].(string); r.JobLike && !Finished(st) {
			return nil, usagef("%s is %s: cancel it first", label(r, o), st)
		}
		impact, err := c.impact(ctx, r, o)
		if err != nil {
			return nil, err
		}
		path := r.Path + idOf(o) + "/"
		p.Steps = append(p.Steps, &Step{
			Target: label(r, o), Action: "delete", Method: http.MethodDelete, Path: path, Before: snapshot(o), Impact: impact, Status: "planned",
			verify: func(ctx context.Context, c *Client, _ Object) error {
				left, err := c.Get(ctx, path)
				if errors.Is(err, ErrNotFound) || (err == nil && left["pending_deletion"] == true) {
					return nil
				}
				if err == nil {
					return errors.New("the object is still there")
				}
				return err
			},
		})
	}
	return p, nil
}

// impact says what a delete takes with it or leaves broken.
func (c *Client) impact(ctx context.Context, r *Resource, o Object) (string, error) {
	count := func(path string, q url.Values) (int, error) {
		_, n, err := c.List(ctx, path, q, 1)
		return n, err
	}
	id := idOf(o)
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	switch r.Kind {
	case "inventories":
		add(asInt(o["total_hosts"]), "hosts deleted")
		add(asInt(o["total_groups"]), "groups deleted")
		n, err := count("/job_templates/", url.Values{"inventory": {id}})
		if err != nil {
			return "", err
		}
		add(n, "job templates left without an inventory")
	case "projects":
		n, err := count("/job_templates/", url.Values{"project": {id}})
		if err != nil {
			return "", err
		}
		add(n, "job templates left without a project")
	case "job-templates", "workflow-templates":
		n, err := count("/schedules/", url.Values{"unified_job_template": {id}})
		if err != nil {
			return "", err
		}
		add(n, "schedules deleted")
	case "organizations":
		if counts, ok := Lookup(o, "summary_fields.related_field_counts").(map[string]any); ok {
			for _, k := range []string{"inventories", "projects", "job_templates", "teams"} {
				add(asInt(counts[k]), strings.ReplaceAll(k, "_", " ")+" deleted")
			}
		}
	case "groups":
		n, err := count(r.Path+id+"/all_hosts/", nil)
		if err != nil {
			return "", err
		}
		add(n, "hosts lose this group (the hosts stay)")
	case "credentials":
		n, err := count("/job_templates/", url.Values{"credentials": {id}})
		if err != nil {
			return "", err
		}
		add(n, "job templates lose this credential")
	}
	return strings.Join(parts, "; "), nil
}

// PlanSetEnabled builds enabling or disabling hosts or schedules.
func (c *Client) PlanSetEnabled(ctx context.Context, r *Resource, refs []string, scope url.Values, enabled bool) (*Plan, error) {
	p, err := c.PlanUpdate(ctx, r, refs, scope, map[string]any{"enabled": enabled})
	if err != nil {
		return nil, err
	}
	verb := "disable"
	if enabled {
		verb = "enable"
	}
	p.Command, p.Tier = verb+" "+r.Singular, "operator"
	for _, s := range p.Steps {
		s.Action = verb
	}
	return p, nil
}

// relations lists what can be attached to what: parent kind -> child kind -> the
// sub-path under the parent.
var relations = map[string]map[string]string{
	"groups":             {"hosts": "hosts", "groups": "children"},
	"hosts":              {"groups": "groups"},
	"job-templates":      {"credentials": "credentials", "labels": "labels", "instance-groups": "instance_groups"},
	"workflow-templates": {"labels": "labels"},
	"inventories":        {"instance-groups": "instance_groups"},
}

// Relations describes what attach and detach accept, for help and the manifest.
func Relations() []string {
	var out []string
	for parent, kids := range relations {
		for kid := range kids {
			out = append(out, parent+" <- "+kid)
		}
	}
	sort.Strings(out)
	return out
}

// PlanAttach builds attaching children to a parent (hosts to a group, a
// credential to a job template), or detaching them.
func (c *Client) PlanAttach(ctx context.Context, parent *Resource, parentRef string, child *Resource, childRefs []string, scope url.Values, detach bool) (*Plan, error) {
	sub, ok := relations[parent.Kind][child.Kind]
	if !ok {
		return nil, usagef("%s cannot be attached to %s; possible: %s", child.Kind, parent.Kind, strings.Join(Relations(), ", "))
	}
	po, err := c.Resolve(ctx, parent, parentRef, scope)
	if err != nil {
		return nil, err
	}
	// A host or group must be in the parent's own inventory.
	childScope := url.Values{}
	if child.Scope != "" && po[child.Scope] != nil {
		childScope.Set(child.Scope, fmt.Sprint(asInt(po[child.Scope])))
	}
	verb := "attach"
	if detach {
		verb = "detach"
	}
	path := parent.Path + idOf(po) + "/" + sub + "/"
	p := &Plan{Command: verb, Tier: "write"}
	for _, ref := range childRefs {
		co, err := c.Resolve(ctx, child, ref, childScope)
		if err != nil {
			return nil, err
		}
		_, n, err := c.List(ctx, path, url.Values{"id": {idOf(co)}}, 1)
		if err != nil {
			return nil, err
		}
		if (n > 0) != detach {
			continue // already as wanted
		}
		body := map[string]any{"id": ID(co)}
		if detach {
			body["disassociate"] = true
		}
		p.Steps = append(p.Steps, &Step{
			Target: label(child, co) + " on " + label(parent, po), Action: verb, Method: http.MethodPost, Path: path,
			Send: body, body: body, Before: map[string]any{"attached": n > 0}, Status: "planned",
			verify: func(ctx context.Context, c *Client, _ Object) error {
				_, now, err := c.List(ctx, path, url.Values{"id": {idOf(co)}}, 1)
				if err != nil {
					return err
				}
				if (now > 0) == detach {
					return errors.New("the membership did not change")
				}
				return nil
			},
		})
	}
	return p, nil
}

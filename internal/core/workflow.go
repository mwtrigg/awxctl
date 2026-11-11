package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// GraphNode is one node of a workflow in the short form people and agents edit:
// what it runs and which nodes follow it.
type GraphNode struct {
	ID       string         `json:"id" yaml:"id"`                                 // the node's identifier, unique in the workflow
	Template string         `json:"template,omitempty" yaml:"template,omitempty"` // what it runs, by name
	Type     string         `json:"type,omitempty" yaml:"type,omitempty"`         // job, workflow, project, inventory-source or approval
	Approval *GraphApproval `json:"approval,omitempty" yaml:"approval,omitempty"` // for type approval
	// Missing marks a node whose template was deleted: it does nothing when the
	// workflow runs. It can be kept as it is or removed, not created.
	Missing    bool           `json:"missing_template,omitempty" yaml:"missing_template,omitempty"`
	OnSuccess  []string       `json:"on_success,omitempty" yaml:"on_success,omitempty"` // node IDs that run if this one succeeds
	OnFailure  []string       `json:"on_failure,omitempty" yaml:"on_failure,omitempty"` // ... if it fails
	Always     []string       `json:"always,omitempty" yaml:"always,omitempty"`         // ... either way
	AllParents bool           `json:"all_parents_must_converge,omitempty" yaml:"all_parents_must_converge,omitempty"`
	Limit      string         `json:"limit,omitempty" yaml:"limit,omitempty"`
	ExtraData  map[string]any `json:"extra_data,omitempty" yaml:"extra_data,omitempty"`
	nodeID     int            // the controller's numeric ID, when the node exists
	templateID int
}

// GraphApproval describes an approval step.
type GraphApproval struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Timeout     int    `json:"timeout,omitempty" yaml:"timeout,omitempty"` // seconds; 0 = waits forever
}

var graphTypes = map[string]string{"job": "job", "workflow_job": "workflow", "project_update": "project", "inventory_update": "inventory-source",
	"workflow_approval": "approval", "system_job": "system"}

var graphKinds = map[string]string{"job": "job-templates", "workflow": "workflow-templates", "project": "projects", "inventory-source": "inventory-sources"}

var linkFields = []struct{ api, name string }{{"success_nodes", "on_success"}, {"failure_nodes", "on_failure"}, {"always_nodes", "always"}}

func (n *GraphNode) links(api string) *[]string {
	switch api {
	case "success_nodes":
		return &n.OnSuccess
	case "failure_nodes":
		return &n.OnFailure
	}
	return &n.Always
}

// ReadGraph reads the nodes of a workflow template in the short form.
func (c *Client) ReadGraph(ctx context.Context, wf Object) ([]GraphNode, error) {
	raw, _, err := c.List(ctx, "/workflow_job_templates/"+idOf(wf)+"/workflow_nodes/", url.Values{"order_by": {"id"}}, 0)
	if err != nil {
		return nil, err
	}
	ident := map[int]string{}
	for _, n := range raw {
		id, _ := n["identifier"].(string)
		if id == "" {
			id = "node-" + idOf(n)
		}
		ident[ID(n)] = id
	}
	nodes := make([]GraphNode, 0, len(raw))
	for _, n := range raw {
		g := GraphNode{ID: ident[ID(n)], nodeID: ID(n), templateID: asInt(n["unified_job_template"]), AllParents: n["all_parents_must_converge"] == true}
		g.Template, _ = Lookup(n, "summary_fields.unified_job_template.name").(string)
		ujt, _ := Lookup(n, "summary_fields.unified_job_template.unified_job_type").(string)
		g.Type = graphTypes[ujt]
		g.Missing = g.templateID == 0
		g.Limit, _ = n["limit"].(string)
		if ed, ok := n["extra_data"].(map[string]any); ok && len(ed) > 0 {
			g.ExtraData = maskMap(ed)
		}
		if g.Type == "approval" {
			g.Approval = &GraphApproval{Name: g.Template}
			if at, err := c.Get(ctx, "/workflow_approval_templates/"+fmt.Sprint(g.templateID)+"/"); err == nil {
				g.Approval.Description, _ = at["description"].(string)
				g.Approval.Timeout = asInt(at["timeout"])
			}
			g.Template = ""
		}
		for _, lf := range linkFields {
			if list, ok := n[lf.api].([]any); ok {
				for _, child := range list {
					*g.links(lf.api) = append(*g.links(lf.api), ident[asInt(child)])
				}
				sort.Strings(*g.links(lf.api))
			}
		}
		nodes = append(nodes, g)
	}
	return nodes, nil
}

func validateGraph(nodes []GraphNode) error {
	byID := map[string]*GraphNode{}
	for i := range nodes {
		n := &nodes[i]
		if strings.TrimSpace(n.ID) == "" || strings.ContainsAny(n.ID, "\n\r") || len(n.ID) > 512 {
			return usagef("node %d: id is required (one line, at most 512 characters)", i+1)
		}
		if byID[n.ID] != nil {
			return usagef("the node id %q is used twice", n.ID)
		}
		byID[n.ID] = n
		switch {
		case n.Type == "approval" || n.Approval != nil:
			if n.Approval == nil || n.Approval.Name == "" || n.Template != "" {
				return usagef("node %q: an approval node has approval: {name: ...} and no template", n.ID)
			}
			n.Type = "approval"
		case n.Missing:
			if n.Template != "" {
				return usagef("node %q: give it a template or missing_template: true, not both", n.ID)
			}
		case n.Template == "":
			return usagef("node %q: template is required (or approval: for an approval step)", n.ID)
		case n.Type != "" && graphKinds[n.Type] == "":
			return usagef("node %q: type is job, workflow, project, inventory-source or approval", n.ID)
		}
	}
	for _, n := range nodes {
		for _, lf := range linkFields {
			seen := map[string]bool{}
			for _, child := range *n.links(lf.api) {
				if byID[child] == nil {
					return usagef("node %q: %s names %q, which is not a node", n.ID, lf.name, child)
				}
				if child == n.ID || seen[child] {
					return usagef("node %q: %s lists %q twice or points at itself", n.ID, lf.name, child)
				}
				seen[child] = true
			}
		}
	}
	// no cycles
	state := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return usagef("the graph has a cycle through node %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		n := byID[id]
		for _, lf := range linkFields {
			for _, child := range *n.links(lf.api) {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		return nil
	}
	for _, n := range nodes {
		if err := visit(n.ID); err != nil {
			return err
		}
	}
	return nil
}

// PlanGraph builds the steps that turn a workflow template's nodes into the
// wanted graph: links and nodes that are not wanted are removed, missing nodes
// are created, changed ones updated, missing links added. Nodes are matched by
// their id, so a node that keeps its id keeps its history.
func (c *Client) PlanGraph(ctx context.Context, wfRef string, want []GraphNode) (*Plan, error) {
	if err := validateGraph(want); err != nil {
		return nil, err
	}
	wfr := FindResource("workflow-templates")
	wf, err := c.Resolve(ctx, wfr, wfRef, nil)
	if err != nil {
		return nil, err
	}
	have, err := c.ReadGraph(ctx, wf)
	if err != nil {
		return nil, err
	}
	haveByID := map[string]*GraphNode{}
	ids := map[string]int{} // node id -> numeric ID; filled in as nodes are created
	for i := range have {
		haveByID[have[i].ID] = &have[i]
		ids[have[i].ID] = have[i].nodeID
	}
	wantByID := map[string]*GraphNode{}
	for i := range want {
		n := &want[i]
		wantByID[n.ID] = n
		if n.Type == "approval" {
			continue
		}
		if n.Missing {
			if h := haveByID[n.ID]; h == nil || !h.Missing {
				return nil, usagef("node %q: missing_template only describes an existing node whose template was deleted: give this node a template", n.ID)
			}
			continue
		}
		var tmpl Object
		if kind := graphKinds[n.Type]; kind != "" {
			tmpl, err = c.Resolve(ctx, FindResource(kind), n.Template, nil)
		} else {
			tmpl, err = c.Resolve(ctx, unifiedTemplates, n.Template, nil)
			var amb *AmbiguousError
			if errors.As(err, &amb) {
				return nil, usagef("node %q: more than one template is named %q: add type: job, workflow, project or inventory-source", n.ID, n.Template)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.ID, err)
		}
		if ID(tmpl) == ID(wf) && (n.Type == "workflow" || tmpl["type"] == "workflow_job_template") {
			return nil, usagef("node %q runs the workflow itself", n.ID)
		}
		n.templateID = ID(tmpl)
	}
	wfLabel := label(wfr, wf)
	p := &Plan{Command: "set workflow-graph", Tier: "write"}
	nodePath := func(id string) string {
		if n := ids[id]; n != 0 {
			return fmt.Sprintf("/workflow_job_template_nodes/%d/", n)
		}
		return "/workflow_job_template_nodes/<new " + id + ">/"
	}
	link := func(parent, api, name, child string, remove bool) *Step {
		verb, body := "link", map[string]any{"id": "<" + child + ">"}
		if remove {
			verb = "unlink"
			body["disassociate"] = true
		}
		return &Step{Target: fmt.Sprintf("%s: %s -[%s]-> %s", wfLabel, parent, name, child), Action: verb, Method: http.MethodPost,
			Path: nodePath(parent) + api + "/", Send: body, Status: "planned",
			apply: func(ctx context.Context, c *Client) (Object, error) {
				pid, cid := ids[parent], ids[child]
				if pid == 0 || cid == 0 {
					return nil, fmt.Errorf("node %q or %q does not exist (an earlier step failed)", parent, child)
				}
				b := map[string]any{"id": cid}
				if remove {
					b["disassociate"] = true
				}
				return c.Send(ctx, http.MethodPost, fmt.Sprintf("/workflow_job_template_nodes/%d/%s/", pid, api), b)
			}}
	}
	sortedIDs := func(m map[string]*GraphNode) []string {
		out := make([]string, 0, len(m))
		for id := range m {
			out = append(out, id)
		}
		sort.Strings(out)
		return out
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}

	// 1. links that are no longer wanted (between nodes that stay)
	for _, id := range sortedIDs(haveByID) {
		h, w := haveByID[id], wantByID[id]
		if w == nil {
			continue // the node goes, and its links with it
		}
		for _, lf := range linkFields {
			for _, child := range *h.links(lf.api) {
				if wantByID[child] != nil && !has(*w.links(lf.api), child) {
					p.Steps = append(p.Steps, link(id, lf.api, lf.name, child, true))
				}
			}
		}
	}
	// 2. nodes that are no longer wanted
	for _, id := range sortedIDs(haveByID) {
		if wantByID[id] != nil {
			continue
		}
		h := haveByID[id]
		p.Steps = append(p.Steps, &Step{Target: fmt.Sprintf("%s: node %s", wfLabel, id), Action: "delete node", Method: http.MethodDelete, Path: nodePath(id),
			Before: map[string]any{"template": h.Template, "type": h.Type, "on_success": h.OnSuccess, "on_failure": h.OnFailure, "always": h.Always}, Status: "planned"})
	}
	// 3. new nodes, 4. changed nodes
	for _, n := range want {
		n := n
		h := haveByID[n.ID]
		fields := map[string]any{"all_parents_must_converge": n.AllParents}
		if n.Type != "approval" && !n.Missing {
			fields["unified_job_template"] = n.templateID
		}
		if n.Limit != "" || (h != nil && h.Limit != "") {
			fields["limit"] = n.Limit
		}
		if n.ExtraData != nil {
			for k, v := range n.ExtraData {
				if v == "******" {
					return nil, usagef("node %q: extra_data.%s is the mask ******: give the real value", n.ID, k)
				}
			}
			fields["extra_data"] = n.ExtraData
		}
		approval := map[string]any{}
		if n.Approval != nil {
			approval = map[string]any{"name": n.Approval.Name, "description": n.Approval.Description, "timeout": n.Approval.Timeout}
		}
		target := fmt.Sprintf("%s: node %s", wfLabel, n.ID)
		if h == nil {
			body := map[string]any{"identifier": n.ID}
			for k, v := range fields {
				body[k] = v
			}
			shown := maskMap(body)
			if n.Type == "approval" {
				shown["approval"] = approval
			} else {
				shown["template"] = n.Template
			}
			p.Steps = append(p.Steps, &Step{Target: target, Action: "create node", Method: http.MethodPost, Path: "/workflow_job_templates/" + idOf(wf) + "/workflow_nodes/",
				Send: shown, Status: "planned",
				apply: func(ctx context.Context, c *Client) (Object, error) {
					res, err := c.Send(ctx, http.MethodPost, "/workflow_job_templates/"+idOf(wf)+"/workflow_nodes/", body)
					if err != nil {
						return nil, err
					}
					if ID(res) == 0 {
						return nil, &OutcomeUnknownError{Method: http.MethodPost, Path: "workflow_nodes", Cause: errors.New("the answer names no node")}
					}
					ids[n.ID] = ID(res)
					if n.Type == "approval" {
						if _, err := c.Send(ctx, http.MethodPost, fmt.Sprintf("/workflow_job_template_nodes/%d/create_approval_template/", ID(res)), approval); err != nil {
							return nil, fmt.Errorf("the node was created but its approval step was not: %w", err)
						}
					}
					return res, nil
				},
				result: func(res Object) map[string]any { return map[string]any{"node_id": ID(res)} }})
			continue
		}
		if (h.Type == "approval") != (n.Type == "approval") {
			return nil, usagef("node %q changes between an approval and a template: give the new node a new id", n.ID)
		}
		change, before := map[string]any{}, map[string]any{}
		if n.Type != "approval" && !n.Missing && h.templateID != n.templateID {
			change["unified_job_template"], before["template"] = n.templateID, h.Template
		}
		if h.AllParents != n.AllParents {
			change["all_parents_must_converge"], before["all_parents_must_converge"] = n.AllParents, h.AllParents
		}
		if h.Limit != n.Limit {
			change["limit"], before["limit"] = n.Limit, h.Limit
		}
		if n.ExtraData != nil && !same(maskMap(n.ExtraData), h.ExtraData) {
			change["extra_data"], before["extra_data"] = n.ExtraData, h.ExtraData
		}
		if len(change) > 0 {
			shown := maskMap(change)
			if _, ok := change["unified_job_template"]; ok {
				shown["template"] = n.Template
			}
			p.Steps = append(p.Steps, &Step{Target: target, Action: "update node", Method: http.MethodPatch, Path: nodePath(n.ID), Send: shown, body: change, Before: before, Status: "planned"})
		}
		if n.Approval != nil && h.Approval != nil && *n.Approval != *h.Approval {
			p.Steps = append(p.Steps, &Step{Target: target, Action: "update approval", Method: http.MethodPatch, Path: fmt.Sprintf("/workflow_approval_templates/%d/", h.templateID),
				Send: approval, body: approval, Before: map[string]any{"name": h.Approval.Name, "description": h.Approval.Description, "timeout": h.Approval.Timeout}, Status: "planned"})
		}
	}
	// 5. links that are missing
	for _, n := range want {
		h := haveByID[n.ID]
		for _, lf := range linkFields {
			for _, child := range *n.links(lf.api) {
				if h != nil && haveByID[child] != nil && has(*h.links(lf.api), child) {
					continue
				}
				p.Steps = append(p.Steps, link(n.ID, lf.api, lf.name, child, false))
			}
		}
	}
	return p, nil
}

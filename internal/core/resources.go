package core

import (
	"encoding/json"
	"sort"
	"strings"
)

// Field is one flat output field of a resource. From is the dotted path in the
// API object (default: Key). Wide fields are left out of tables.
type Field struct {
	Key    string
	Header string
	From   string
	Wide   bool
}

// Resource describes one kind of object: where it lives, how a row is built and
// what may be written. The CLI, the MCP server and the TUI all read this.
type Resource struct {
	Kind      string   // plural, as typed: "job-templates"
	Singular  string   // "job template"
	Aliases   []string // other accepted spellings
	Path      string   // collection path under the API prefix: "/job_templates/"
	NameField string   // "name", "username", "hostname"; "" = addressed by ID only
	Order     string   // default order_by
	TimeField string   // field --since/--until filter on; "" = none
	JobLike   bool     // a run: has a name, a status and failed, so --failed and --name apply
	Fields    []Field
	// Filters are convenience flags: flag name -> query parameter.
	Filters map[string]string
	// Writable lists the fields create/update accept; nil = read-only here.
	Writable []string
	// Refs maps a writable field to the kind its value names, so a name can be
	// given instead of an ID.
	Refs map[string]string
	// Scope is the field that narrows a name lookup (hosts and groups live in an
	// inventory).
	Scope string
	// Secret marks objects whose values must never be shown (credentials).
	Secret bool
}

func f(key, header string) Field        { return Field{Key: key, Header: header} }
func ff(key, header, from string) Field { return Field{Key: key, Header: header, From: from} }
func w(key string) Field                { return Field{Key: key, Header: strings.ToUpper(key), Wide: true} }
func wf(key, from string) Field {
	return Field{Key: key, Header: strings.ToUpper(key), From: from, Wide: true}
}

var jobFields = []Field{
	f("id", "ID"), f("name", "NAME"), f("status", "STATUS"), f("launch_type", "LAUNCHED"),
	ff("launched_by", "BY", "launched_by.name"),
	f("started", "STARTED"), f("finished", "FINISHED"), f("elapsed", "SECONDS"),
	w("failed"), w("created"), w("job_explanation"), wf("launched_by_type", "launched_by.type"),
}

func withJob(extra ...Field) []Field { return append(append([]Field{}, jobFields...), extra...) }

var resources = []*Resource{
	{Kind: "jobs", JobLike: true, Singular: "job", Path: "/jobs/", Order: "-id", TimeField: "created",
		Fields: withJob(
			ff("template", "TEMPLATE", "summary_fields.job_template.name"),
			ff("inventory", "INVENTORY", "summary_fields.inventory.name"),
			f("limit", "LIMIT"), w("job_type"), w("playbook"), w("scm_revision"), w("job_tags"), w("skip_tags"), w("extra_vars"),
			wf("project", "summary_fields.project.name"),
			wf("execution_environment", "summary_fields.execution_environment.name"),
			wf("template_id", "job_template"), wf("inventory_id", "inventory"), w("execution_node"),
		),
		Filters: map[string]string{"template": "job_template__name", "inventory": "inventory__name", "project": "project__name", "status": "status", "launched-by": "created_by__username"}},
	{Kind: "unified-jobs", JobLike: true, Singular: "unified job", Aliases: []string{"all-jobs"}, Path: "/unified_jobs/", Order: "-id", TimeField: "created",
		Fields:  withJob(f("type", "TYPE"), wf("template", "summary_fields.unified_job_template.name")),
		Filters: map[string]string{"status": "status", "type": "type", "template": "unified_job_template__name"}},
	{Kind: "workflow-jobs", JobLike: true, Singular: "workflow job", Path: "/workflow_jobs/", Order: "-id", TimeField: "created",
		Fields:  withJob(ff("template", "TEMPLATE", "summary_fields.workflow_job_template.name"), w("limit")),
		Filters: map[string]string{"template": "workflow_job_template__name", "status": "status"}},
	{Kind: "project-updates", JobLike: true, Singular: "project update", Path: "/project_updates/", Order: "-id", TimeField: "created",
		Fields:  withJob(ff("project", "PROJECT", "summary_fields.project.name"), w("scm_revision"), w("scm_branch")),
		Filters: map[string]string{"project": "project__name", "status": "status"}},
	{Kind: "inventory-updates", JobLike: true, Singular: "inventory update", Path: "/inventory_updates/", Order: "-id", TimeField: "created",
		Fields:  withJob(ff("inventory", "INVENTORY", "summary_fields.inventory.name"), ff("source", "SOURCE", "summary_fields.inventory_source.name")),
		Filters: map[string]string{"inventory": "inventory__name", "status": "status"}},
	{Kind: "ad-hoc-commands", JobLike: true, Singular: "ad hoc command", Path: "/ad_hoc_commands/", Order: "-id", TimeField: "created",
		Fields: withJob(f("module_name", "MODULE"), ff("inventory", "INVENTORY", "summary_fields.inventory.name"), f("limit", "LIMIT"))},
	{Kind: "system-jobs", JobLike: true, Singular: "system job", Path: "/system_jobs/", Order: "-id", TimeField: "created", Fields: withJob(w("job_type"))},
	{Kind: "workflow-approvals", Singular: "workflow approval", Aliases: []string{"approvals"}, Path: "/workflow_approvals/", Order: "-id", TimeField: "created",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("status", "STATUS"),
			ff("workflow_job", "WORKFLOW JOB", "summary_fields.workflow_job.name"), ff("workflow_job_id", "WF JOB ID", "summary_fields.workflow_job.id"),
			f("started", "STARTED"), f("approval_expiration", "EXPIRES"), w("timed_out"), w("description"), w("finished")},
		Filters: map[string]string{"status": "status"}},

	{Kind: "job-templates", Singular: "job template", Aliases: []string{"templates"}, Path: "/job_templates/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("organization", "ORG", "summary_fields.organization.name"),
			ff("inventory", "INVENTORY", "summary_fields.inventory.name"), ff("project", "PROJECT", "summary_fields.project.name"),
			f("playbook", "PLAYBOOK"), f("status", "LAST STATUS"), f("last_job_run", "LAST RUN"), f("next_job_run", "NEXT RUN"),
			w("description"), w("job_type"), w("limit"), w("job_tags"), w("skip_tags"), w("forks"), w("verbosity"), w("survey_enabled"), w("last_job_failed"),
			wf("execution_environment", "summary_fields.execution_environment.name"),
			w("ask_limit_on_launch"), w("ask_variables_on_launch"), w("ask_inventory_on_launch"), w("ask_tags_on_launch"),
			w("ask_skip_tags_on_launch"), w("ask_job_type_on_launch"), w("ask_credential_on_launch"), w("ask_scm_branch_on_launch"),
			w("allow_simultaneous"), w("become_enabled"), w("extra_vars")},
		Filters: map[string]string{"organization": "organization__name", "inventory": "inventory__name", "project": "project__name", "playbook": "playbook"},
		Writable: []string{"name", "description", "job_type", "inventory", "project", "playbook", "organization", "limit", "job_tags", "skip_tags", "forks", "verbosity",
			"extra_vars", "execution_environment", "become_enabled", "allow_simultaneous", "scm_branch", "timeout", "diff_mode",
			"ask_limit_on_launch", "ask_variables_on_launch", "ask_inventory_on_launch", "ask_tags_on_launch", "ask_skip_tags_on_launch",
			"ask_job_type_on_launch", "ask_credential_on_launch", "ask_scm_branch_on_launch", "ask_verbosity_on_launch", "ask_diff_mode_on_launch"},
		Refs: map[string]string{"inventory": "inventories", "project": "projects", "organization": "organizations", "execution_environment": "execution-environments"}},
	{Kind: "workflow-templates", Singular: "workflow template", Aliases: []string{"workflow-job-templates", "workflows"}, Path: "/workflow_job_templates/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("organization", "ORG", "summary_fields.organization.name"),
			f("status", "LAST STATUS"), f("last_job_run", "LAST RUN"), f("next_job_run", "NEXT RUN"),
			w("description"), w("survey_enabled"), w("last_job_failed"), w("limit"), w("extra_vars"), w("allow_simultaneous"),
			w("ask_limit_on_launch"), w("ask_variables_on_launch"), w("ask_inventory_on_launch"), wf("inventory", "summary_fields.inventory.name")},
		Filters:  map[string]string{"organization": "organization__name"},
		Writable: []string{"name", "description", "organization", "inventory", "limit", "extra_vars", "allow_simultaneous", "scm_branch", "ask_limit_on_launch", "ask_variables_on_launch", "ask_inventory_on_launch"},
		Refs:     map[string]string{"organization": "organizations", "inventory": "inventories"}},

	{Kind: "inventories", Singular: "inventory", Path: "/inventories/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("organization", "ORG", "summary_fields.organization.name"), f("kind", "KIND"),
			f("total_hosts", "HOSTS"), f("hosts_with_active_failures", "FAILING"), f("total_groups", "GROUPS"),
			f("has_inventory_sources", "SOURCES"), w("inventory_sources_with_failures"), w("description"), w("variables"), w("pending_deletion")},
		Filters:  map[string]string{"organization": "organization__name", "kind": "kind"},
		Writable: []string{"name", "description", "organization", "variables", "kind", "host_filter", "prevent_instance_group_fallback"},
		Refs:     map[string]string{"organization": "organizations"}},
	{Kind: "hosts", Singular: "host", Path: "/hosts/", NameField: "name", Order: "name", Scope: "inventory",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("inventory", "INVENTORY", "summary_fields.inventory.name"), f("enabled", "ENABLED"),
			f("has_active_failures", "FAILING"), ff("last_job_id", "LAST JOB", "summary_fields.last_job.id"),
			ff("last_job_status", "LAST STATUS", "summary_fields.last_job.status"), ff("last_job_finished", "LAST FINISHED", "summary_fields.last_job.finished"),
			w("description"), w("variables"), wf("inventory_id", "inventory"), wf("groups", "summary_fields.groups.results"), w("instance_id")},
		Filters:  map[string]string{"inventory": "inventory__name", "group": "groups__name", "enabled": "enabled"},
		Writable: []string{"name", "description", "inventory", "enabled", "variables", "instance_id"},
		Refs:     map[string]string{"inventory": "inventories"}},
	{Kind: "groups", Singular: "group", Path: "/groups/", NameField: "name", Order: "name", Scope: "inventory",
		Fields:   []Field{f("id", "ID"), f("name", "NAME"), ff("inventory", "INVENTORY", "summary_fields.inventory.name"), w("description"), w("variables"), wf("inventory_id", "inventory")},
		Filters:  map[string]string{"inventory": "inventory__name", "host": "hosts__name"},
		Writable: []string{"name", "description", "inventory", "variables"},
		Refs:     map[string]string{"inventory": "inventories"}},
	{Kind: "inventory-sources", Singular: "inventory source", Path: "/inventory_sources/", NameField: "name", Order: "name", Scope: "inventory",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("inventory", "INVENTORY", "summary_fields.inventory.name"), f("source", "SOURCE"),
			f("status", "STATUS"), f("last_updated", "LAST UPDATED"), w("update_on_launch"), w("overwrite"), w("overwrite_vars"), w("source_path"),
			wf("source_project", "summary_fields.source_project.name"), w("description"), w("last_job_failed"), w("next_job_run")},
		Filters:  map[string]string{"inventory": "inventory__name", "source": "source", "status": "status"},
		Writable: []string{"name", "description", "inventory", "source", "source_project", "source_path", "source_vars", "update_on_launch", "overwrite", "overwrite_vars", "update_cache_timeout", "execution_environment", "credential", "enabled_var", "enabled_value", "host_filter"},
		Refs:     map[string]string{"inventory": "inventories", "source_project": "projects", "execution_environment": "execution-environments", "credential": "credentials"}},

	{Kind: "projects", Singular: "project", Path: "/projects/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("organization", "ORG", "summary_fields.organization.name"), f("scm_type", "SCM"),
			f("scm_url", "URL"), f("scm_branch", "BRANCH"), f("status", "STATUS"), f("last_job_run", "LAST SYNC"),
			w("scm_revision"), w("description"), w("scm_update_on_launch"), w("last_job_failed"), w("local_path"), w("scm_refspec"),
			wf("default_environment", "summary_fields.default_environment.name"), wf("credential", "summary_fields.credential.name")},
		Filters: map[string]string{"organization": "organization__name", "status": "status", "scm-type": "scm_type"},
		Writable: []string{"name", "description", "organization", "scm_type", "scm_url", "scm_branch", "scm_refspec", "scm_clean", "scm_delete_on_update",
			"scm_track_submodules", "scm_update_on_launch", "scm_update_cache_timeout", "allow_override", "credential", "default_environment", "local_path", "timeout"},
		Refs: map[string]string{"organization": "organizations", "credential": "credentials", "default_environment": "execution-environments"}},
	{Kind: "credentials", Singular: "credential", Path: "/credentials/", NameField: "name", Order: "name", Secret: true,
		Fields: []Field{f("id", "ID"), f("name", "NAME"), ff("type", "TYPE", "summary_fields.credential_type.name"),
			ff("organization", "ORG", "summary_fields.organization.name"), f("kind", "KIND"), w("description"), w("managed"), w("created"), w("modified")},
		Filters:  map[string]string{"organization": "organization__name", "type": "credential_type__name", "kind": "credential_type__kind"},
		Writable: []string{"name", "description", "organization", "credential_type", "inputs"},
		Refs:     map[string]string{"organization": "organizations", "credential_type": "credential-types"}},
	{Kind: "credential-types", Singular: "credential type", Path: "/credential_types/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("kind", "KIND"), f("managed", "MANAGED"), w("description")}},
	{Kind: "execution-environments", Singular: "execution environment", Aliases: []string{"ees"}, Path: "/execution_environments/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("image", "IMAGE"), ff("organization", "ORG", "summary_fields.organization.name"),
			f("pull", "PULL"), f("managed", "MANAGED"), w("description"), wf("credential", "summary_fields.credential.name")},
		Writable: []string{"name", "description", "image", "organization", "pull", "credential"},
		Refs:     map[string]string{"organization": "organizations", "credential": "credentials"}},
	{Kind: "schedules", Singular: "schedule", Path: "/schedules/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("enabled", "ENABLED"), ff("template", "TEMPLATE", "summary_fields.unified_job_template.name"),
			ff("type", "TYPE", "summary_fields.unified_job_template.unified_job_type"), f("next_run", "NEXT RUN"), f("rrule", "RRULE"),
			w("timezone"), w("dtstart"), w("dtend"), w("description"), w("extra_data"), wf("template_id", "unified_job_template"), w("limit")},
		Filters:  map[string]string{"template": "unified_job_template__name", "enabled": "enabled"},
		Writable: []string{"name", "description", "unified_job_template", "rrule", "enabled", "extra_data", "limit", "job_tags", "skip_tags", "inventory"},
		Refs:     map[string]string{"inventory": "inventories", "unified_job_template": "unified-job-templates"}},

	{Kind: "organizations", Singular: "organization", Aliases: []string{"orgs"}, Path: "/organizations/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("description", "DESCRIPTION"),
			ff("job_templates", "TEMPLATES", "summary_fields.related_field_counts.job_templates"), ff("inventories", "INVENTORIES", "summary_fields.related_field_counts.inventories"),
			ff("projects", "PROJECTS", "summary_fields.related_field_counts.projects"), ff("users", "USERS", "summary_fields.related_field_counts.users"),
			ff("teams", "TEAMS", "summary_fields.related_field_counts.teams"), w("max_hosts")}},
	{Kind: "users", Singular: "user", Path: "/users/", NameField: "username", Order: "username",
		Fields: []Field{f("id", "ID"), f("username", "USERNAME"), f("first_name", "FIRST"), f("last_name", "LAST"), f("email", "EMAIL"),
			f("is_superuser", "SUPERUSER"), ff("is_auditor", "AUDITOR", "is_system_auditor|is_platform_auditor"), f("last_login", "LAST LOGIN"), w("created")},
		Filters: map[string]string{"superuser": "is_superuser"}},
	{Kind: "teams", Singular: "team", Path: "/teams/", NameField: "name", Order: "name",
		Fields:  []Field{f("id", "ID"), f("name", "NAME"), ff("organization", "ORG", "summary_fields.organization.name"), f("description", "DESCRIPTION")},
		Filters: map[string]string{"organization": "organization__name"}},
	{Kind: "tokens", Singular: "token", Path: "/tokens/", Order: "-id", Secret: true,
		Fields: []Field{f("id", "ID"), ff("user", "USER", "summary_fields.user.username"), f("description", "DESCRIPTION"), f("scope", "SCOPE"),
			f("expires", "EXPIRES"), ff("application", "APPLICATION", "summary_fields.application.name"), f("created", "CREATED"), w("last_used"), wf("user_id", "user")},
		Filters: map[string]string{"user": "user__username", "scope": "scope"}},
	{Kind: "role-definitions", Singular: "role definition", Path: "/role_definitions/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("content_type", "APPLIES TO"), f("managed", "MANAGED"), w("description"), w("permissions")}},
	{Kind: "role-user-assignments", Singular: "role user assignment", Path: "/role_user_assignments/", Order: "-id",
		Fields: []Field{f("id", "ID"), ff("user", "USER", "summary_fields.user.username"), ff("role", "ROLE", "summary_fields.role_definition.name"),
			f("content_type", "ON"), ff("object", "OBJECT", "summary_fields.content_object.name"), w("object_id"), w("created")},
		Filters: map[string]string{"user": "user__username", "role": "role_definition__name"}},
	{Kind: "role-team-assignments", Singular: "role team assignment", Path: "/role_team_assignments/", Order: "-id",
		Fields: []Field{f("id", "ID"), ff("team", "TEAM", "summary_fields.team.name"), ff("role", "ROLE", "summary_fields.role_definition.name"),
			f("content_type", "ON"), ff("object", "OBJECT", "summary_fields.content_object.name"), w("object_id"), w("created")},
		Filters: map[string]string{"team": "team__name", "role": "role_definition__name"}},
	{Kind: "notification-templates", Singular: "notification template", Path: "/notification_templates/", NameField: "name", Order: "name", Secret: true,
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("notification_type", "TYPE"), ff("organization", "ORG", "summary_fields.organization.name"), w("description")}},
	{Kind: "notifications", Singular: "notification", Path: "/notifications/", Order: "-id", TimeField: "created",
		Fields:  []Field{f("id", "ID"), f("status", "STATUS"), f("notification_type", "TYPE"), f("subject", "SUBJECT"), f("created", "CREATED"), w("error"), w("recipients")},
		Filters: map[string]string{"status": "status"}},
	{Kind: "labels", Singular: "label", Path: "/labels/", NameField: "name", Order: "name",
		Fields:   []Field{f("id", "ID"), f("name", "NAME"), ff("organization", "ORG", "summary_fields.organization.name")},
		Writable: []string{"name", "organization"},
		Refs:     map[string]string{"organization": "organizations"}},
	{Kind: "activity", Singular: "activity entry", Aliases: []string{"activity-stream"}, Path: "/activity_stream/", Order: "-id", TimeField: "timestamp",
		Fields: []Field{f("id", "ID"), f("timestamp", "TIME"), ff("actor", "ACTOR", "summary_fields.actor.username"), f("operation", "OPERATION"),
			f("object1", "OBJECT"), f("object2", "RELATED"), w("changes"), w("object_association")},
		Filters: map[string]string{"actor": "actor__username", "operation": "operation", "object": "object1"}},
	{Kind: "instances", Singular: "instance", Path: "/instances/", NameField: "hostname", Order: "hostname",
		Fields: []Field{f("id", "ID"), f("hostname", "HOSTNAME"), f("node_type", "TYPE"), f("node_state", "STATE"), f("enabled", "ENABLED"),
			f("capacity", "CAPACITY"), f("consumed_capacity", "USED"), f("jobs_running", "RUNNING"), f("version", "VERSION"), w("last_seen"), w("errors"), w("cpu"), w("memory")}},
	{Kind: "instance-groups", Singular: "instance group", Path: "/instance_groups/", NameField: "name", Order: "name",
		Fields: []Field{f("id", "ID"), f("name", "NAME"), f("capacity", "CAPACITY"), f("consumed_capacity", "USED"), f("jobs_running", "RUNNING"),
			f("instances", "INSTANCES"), f("is_container_group", "CONTAINER")}},
}

// unifiedTemplates is only a lookup target: a schedule can belong to a job
// template, a workflow template, a project or an inventory source.
var unifiedTemplates = &Resource{Kind: "unified-job-templates", Singular: "template", Path: "/unified_job_templates/", NameField: "name"}

// deleteOnly are the kinds that can be deleted but not created or updated here.
var deleteOnly = map[string]bool{"tokens": true, "jobs": true, "workflow-jobs": true, "project-updates": true, "inventory-updates": true, "ad-hoc-commands": true}

// Deletable reports whether objects of this kind can be deleted with awxctl.
func (r *Resource) Deletable() bool { return r.Writable != nil || deleteOnly[r.Kind] }

// Resources returns every kind, sorted by name.
func Resources() []*Resource {
	out := append([]*Resource(nil), resources...)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// FindResource looks a kind up by its name, an alias, its singular or the API
// spelling with underscores.
func FindResource(name string) *Resource {
	n := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
	if n == unifiedTemplates.Kind {
		return unifiedTemplates
	}
	for _, r := range resources {
		if n == r.Kind || n == strings.ReplaceAll(r.Singular, " ", "-") || n == strings.Trim(strings.ReplaceAll(r.Path, "_", "-"), "/") {
			return r
		}
		for _, a := range r.Aliases {
			if n == a {
				return r
			}
		}
	}
	return nil
}

// Row flattens an API object into the resource's output fields.
func (r *Resource) Row(o Object) map[string]any {
	row := make(map[string]any, len(r.Fields))
	for _, fd := range r.Fields {
		from := fd.From
		if from == "" {
			from = fd.Key
		}
		var v any
		for _, alt := range strings.Split(from, "|") { // first path that has a value
			if v = Lookup(o, alt); v != nil {
				break
			}
		}
		if list, ok := v.([]any); ok && strings.HasSuffix(from, ".results") { // summary lists: keep the names
			names := make([]any, 0, len(list))
			for _, it := range list {
				if m, ok := it.(map[string]any); ok && m["name"] != nil {
					names = append(names, m["name"])
				}
			}
			v = names
		}
		if s, ok := v.(string); ok && (fd.Key == "extra_vars" || fd.Key == "extra_data") && strings.HasPrefix(strings.TrimSpace(s), "{") {
			var obj map[string]any // shown as an object, secret-looking values masked
			if json.Unmarshal([]byte(s), &obj) == nil {
				v = Mask(obj)
			}
		} else if m, ok := v.(map[string]any); ok && fd.Key == "extra_data" {
			v = Mask(m)
		} else if text, isText := v.(string); isText && structuredText[fd.Key] {
			v = maskText(text) // variables: secret-looking values hidden (get variables --show-secrets shows them)
		}
		row[fd.Key] = v
	}
	return row
}

// CanWrite reports whether field may be set through create/update.
func (r *Resource) CanWrite(field string) bool {
	for _, wr := range r.Writable {
		if wr == field {
			return true
		}
	}
	return false
}

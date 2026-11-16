package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Step is one change to one object. A plan is built first, with everything
// resolved and nothing sent; only Apply sends.
type Step struct {
	Target string         `json:"target"`           // "host web1 (id 5)"
	Action string         `json:"action"`           // "delete", "launch", "update", ...
	Method string         `json:"method"`           // HTTP method that would be sent
	Path   string         `json:"path"`             // API path under the prefix
	Send   map[string]any `json:"send,omitempty"`   // request body, secrets masked
	Before map[string]any `json:"before,omitempty"` // state before the change, secrets masked
	Impact string         `json:"impact,omitempty"` // what else goes with it (delete cascades)
	Status string         `json:"status"`           // planned, applied, failed, unknown, skipped
	Result map[string]any `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`

	raw    Object
	body   any                                                    // the real body
	verify func(ctx context.Context, c *Client, res Object) error // proves the change, optional
	result func(res Object) map[string]any                        // picks the result fields, optional
	apply  func(ctx context.Context, c *Client) (Object, error)   // overrides the plain request, optional
}

// Raw is the controller's answer to an applied step. It is not part of the plan's
// JSON or of the audit record: a new token's value is only here.
func (s *Step) Raw() Object { return s.raw }

// Plan is what a changing command would do. The CLI prints it, MCP returns it and
// the TUI shows it for confirmation; Apply runs it.
type Plan struct {
	Command string  `json:"command"`
	Tier    string  `json:"-"` // permission tier needed: operator or write
	Steps   []*Step `json:"steps"`
}

// Recorder keeps the audit record of an apply. Begin is called before anything is
// sent, Update after every step.
type Recorder interface {
	Begin(p *Plan) error
	Update(p *Plan) error
}

// ErrSteps reports that some steps of a plan failed.
var ErrSteps = errors.New("some changes failed")

// StepsError says how many steps failed and carries the first failure, so the
// caller still sees what kind it was (denied, not found, ...).
type StepsError struct {
	Failed, Total int
	First         error
}

func (e *StepsError) Error() string {
	if e.Total == 1 {
		return e.First.Error()
	}
	return fmt.Sprintf("%d of %d changes failed; first: %v", e.Failed, e.Total, e.First)
}

// Unwrap exposes both ErrSteps and the first failure to errors.Is and errors.As.
func (e *StepsError) Unwrap() []error { return []error{ErrSteps, e.First} }

// Apply runs the steps in order. A step that fails for sure is marked and the run
// goes on; a step whose outcome is unknown stops the run, since sending more could
// make things worse. A record that cannot be written also stops it.
func (p *Plan) Apply(ctx context.Context, c *Client, rec Recorder) error {
	if rec != nil {
		if err := rec.Begin(p); err != nil {
			return fmt.Errorf("nothing was changed: cannot write the audit record: %w", err)
		}
	}
	failed := 0
	var stop, first error
	for _, s := range p.Steps {
		if stop == nil && ctx.Err() != nil {
			// Interrupted before this step: it was certainly not sent.
			stop = fmt.Errorf("interrupted before %s: %w", s.Target, ctx.Err())
		}
		if stop != nil {
			s.Status = "skipped"
			continue
		}
		var res Object
		var err error
		if s.apply != nil {
			res, err = s.apply(ctx, c)
		} else {
			res, err = c.Send(ctx, s.Method, s.Path, s.body)
		}
		if err == nil && s.verify != nil {
			if verr := s.verify(ctx, c, res); verr != nil {
				err = &OutcomeUnknownError{Method: s.Method, Path: s.Path, Cause: fmt.Errorf("the request was accepted but the change could not be confirmed: %w", verr)}
			}
		}
		if res != nil && s.result != nil {
			s.Result = s.result(res) // also when the change cannot be confirmed: the job ID matters most then
		}
		var unknown *OutcomeUnknownError
		switch {
		case err == nil:
			s.Status, s.raw = "applied", res
		case errors.As(err, &unknown):
			s.Status, s.Error, stop = "unknown", err.Error(), err
		default:
			s.Status, s.Error = "failed", err.Error()
			failed++
			if first == nil {
				first = err
			}
		}
		if rec != nil {
			if uerr := rec.Update(p); uerr != nil && stop == nil {
				stop = fmt.Errorf("stopped after %s: cannot update the audit record: %w", s.Target, uerr)
			}
		}
	}
	if stop != nil {
		if rec != nil {
			_ = rec.Update(p) // the skipped steps
		}
		return stop
	}
	if failed > 0 {
		return &StepsError{Failed: failed, Total: len(p.Steps), First: first}
	}
	return nil
}

// secretKey decides by name what is a secret. It is a heuristic: a secret under a
// name it does not know (the README lists the pattern) is not masked.
var secretKey = regexp.MustCompile(`(?i)pass|pwd|(^|_)pw($|_)|secret|token|private|(^|_)key($|_)|api_?key|credential_inputs|^inputs$|become_|vault|authorize`)

// Mask returns a copy of v with the values of secret-looking keys replaced. It is
// used for everything that is printed or written to an audit file.
func Mask(v any) any {
	switch t := v.(type) {
	case string:
		if isVault(t) {
			return Vaulted
		}
	case map[string]any:
		if isVaultMap(t) {
			return Vaulted
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKey.MatchString(k) && !strings.HasPrefix(k, "passwords_needed") && val != nil && val != "" && val != false {
				out[k] = "******"
				continue
			}
			if text, ok := val.(string); ok && structuredText[k] {
				out[k] = maskText(text)
				continue
			}
			out[k] = Mask(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = Mask(val)
		}
		return out
	}
	return v
}

// Vaulted stands in for an Ansible Vault value. Such a value is encrypted, so it
// is no secret to show, but it is dozens of lines of hex that say nothing.
const Vaulted = "<vaulted>"

// isVault recognises vault text whatever the variable is called.
func isVault(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "$ANSIBLE_VAULT;")
}

// isVaultMap recognises the form the controller's JSON gives a vaulted variable:
// {"__ansible_vault": "$ANSIBLE_VAULT;..."}.
func isVaultMap(m map[string]any) bool {
	if len(m) != 1 {
		return false
	}
	s, ok := m["__ansible_vault"].(string)
	return ok && isVault(s)
}

// TrimVault replaces every vault value in v with the Vaulted marker and leaves
// everything else as it is. It is applied even where secrets are asked to be
// shown: there is nothing to read in a vault blob.
func TrimVault(v any) any {
	switch t := v.(type) {
	case string:
		if isVault(t) {
			return Vaulted
		}
	case map[string]any:
		if isVaultMap(t) {
			return Vaulted
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = TrimVault(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = TrimVault(val)
		}
		return out
	}
	return v
}

// structuredText are the fields whose text is itself a JSON or YAML mapping of
// variables, which can hold secrets.
var structuredText = map[string]bool{"variables": true, "extra_vars": true, "source_vars": true, "extra_data": true}

// maskText masks the secret-looking values inside variables given as text. Text
// that is not a mapping is returned as it is.
func maskText(text string) string {
	var obj map[string]any
	if strings.TrimSpace(text) == "" {
		return text
	}
	if yaml.Unmarshal([]byte(text), &obj) != nil || obj == nil {
		// Not a mapping this parser accepts (the controller's parser is more
		// forgiving). If it still names something secret-looking, hide it all.
		if secretKey.MatchString(text) {
			return "******"
		}
		return text
	}
	masked := Mask(obj).(map[string]any)
	if same(masked, obj) {
		return text // nothing to hide: keep the text exactly as written
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <vaulted> readable
	if err := enc.Encode(masked); err != nil {
		return "******"
	}
	return strings.TrimRight(buf.String(), "\n")
}

func maskMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return Mask(m).(map[string]any)
}

// snapshot is the before-state of an object: its own fields, without the bulky
// related and summary blocks, secrets masked.
func snapshot(o Object) map[string]any {
	out := make(map[string]any, len(o))
	for k, v := range o {
		if k == "related" || k == "summary_fields" {
			continue
		}
		out[k] = v
	}
	return maskMap(out)
}

// FileRecorder writes audit records as JSON files in a private directory.
type FileRecorder struct {
	Dir  string // created 0700 when missing
	Keep int    // newest files to keep; 0 = keep all
	Meta map[string]any
	path string
}

// Path is the record's file ("" before Begin).
func (r *FileRecorder) Path() string { return r.path }

// Begin writes the record with every step still planned.
func (r *FileRecorder) Begin(p *Plan) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	if err := CheckPrivateDir(r.Dir); err != nil {
		return err
	}
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s-%s.json", time.Now().UTC().Format("20060102T150405Z"), strings.ReplaceAll(p.Command, " ", "-"), hex.EncodeToString(b))
	r.path = filepath.Join(r.Dir, name)
	return r.Update(p)
}

// Update rewrites the record atomically.
func (r *FileRecorder) Update(p *Plan) error {
	doc := map[string]any{"command": p.Command, "written": time.Now().UTC().Format(time.RFC3339), "steps": p.Steps}
	for k, v := range r.Meta {
		doc[k] = v
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.Dir, ".record-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	// The record is evidence: it must be on disk, not only in the cache, before
	// the change it describes is sent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return err
	}
	if dir, err := os.Open(r.Dir); err == nil { // make the rename durable too, where the system allows it
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

// Prune deletes the oldest records beyond Keep. Records of an unknown outcome are
// never deleted: someone still has to look at them.
func (r *FileRecorder) Prune() {
	if r.Keep <= 0 {
		return
	}
	names, err := RecordNames(r.Dir)
	if err != nil {
		return
	}
	for i, n := range names { // newest first
		if i < r.Keep {
			continue
		}
		full := filepath.Join(r.Dir, n)
		// A record with a step that is unknown or still planned (the run was
		// interrupted) is evidence someone has yet to look at: it is never pruned.
		data, err := os.ReadFile(full)
		if err != nil || strings.Contains(string(data), `"status": "unknown"`) || strings.Contains(string(data), `"status": "planned"`) {
			continue
		}
		_ = os.Remove(full)
	}
}

var recordName = regexp.MustCompile(`^\d{8}T\d{6}Z-.+-[0-9a-f]{8}\.json$`)

// RecordNames lists the record files of a directory, newest first.
func RecordNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && recordName.MatchString(e.Name()) { // only awxctl's own records, never another .json
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// PlanRaw builds one arbitrary changing request (the `api` escape hatch). The
// before-state is whatever a GET of the same path returns.
func (c *Client) PlanRaw(ctx context.Context, method, rel string, body map[string]any) (*Plan, error) {
	step := &Step{Target: rel, Action: strings.ToLower(method), Method: method, Path: rel, Send: maskMap(body), Status: "planned"}
	if body != nil {
		step.body = body
	}
	if method != "POST" {
		before, err := c.Get(ctx, rel)
		if err != nil {
			return nil, err
		}
		step.Before = snapshot(before)
	}
	step.result = func(res Object) map[string]any {
		if res == nil {
			return nil
		}
		out := map[string]any{}
		for _, k := range []string{"id", "name", "status", "type", "job", "url"} {
			if v, ok := res[k]; ok {
				out[k] = v
			}
		}
		return out
	}
	return &Plan{Command: "api " + strings.ToLower(method), Tier: "write", Steps: []*Step{step}}, nil
}

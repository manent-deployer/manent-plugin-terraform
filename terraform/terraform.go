// Package terraform implements the Terraform deploy plugin of manent: the artifact is a root
// module, every target is a state (a workspace or a state key in a remote backend) and a deploy
// is init + plan + apply of that plan. OpenTofu is the default binary.
package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Name is the plugin name and the artifact type it handles by default.
const Name = "terraform"

// Limits of what the plugin reports.
const (
	maxAddresses = 50
	maxErrorText = 2000
)

// outputKeyRe are the output keys manent accepts.
var outputKeyRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// Plugin is the Terraform deployer.
type Plugin struct {
	logger         *slog.Logger
	types          []string
	binary         string
	pluginCacheDir string
	providerMirror string
	run            Runner
}

// Factory builds the plugin from its configuration (plugins.<name>):
//
//	artifact_types:   artifact types handled (default [terraform])
//	binary:           tofu (default) or terraform, a name looked up in PATH or an absolute path
//	plugin_cache_dir: provider cache shared by the runs of this worker (absolute path)
//	provider_mirror:  URL of a provider network mirror, for isolated environments
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Plugin{logger: logger, types: []string{Name}, binary: "tofu", run: execRunner{}}
	for k, v := range cfg {
		switch k {
		case "artifact_types":
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return nil, errors.New("terraform plugin: artifact_types must be a non-empty list of strings")
			}
			p.types = p.types[:0]
			for _, t := range list {
				s, ok := t.(string)
				if !ok || s == "" {
					return nil, errors.New("terraform plugin: artifact_types must contain strings")
				}
				p.types = append(p.types, s)
			}
		case "binary", "plugin_cache_dir", "provider_mirror":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("terraform plugin: %s must be a non-empty string", k)
			}
			switch k {
			case "binary":
				p.binary = s
			case "plugin_cache_dir":
				if !filepath.IsAbs(s) {
					return nil, errors.New("terraform plugin: plugin_cache_dir must be an absolute path")
				}
				p.pluginCacheDir = s
			case "provider_mirror":
				if !strings.HasPrefix(s, "https://") {
					return nil, errors.New("terraform plugin: provider_mirror must be an https URL")
				}
				p.providerMirror = s
			}
		default:
			// Keys of manent's plugin configuration (external, queue, ...) are not the plugin's.
		}
	}
	return p, nil
}

// Name implements plugin.Deployer.
func (p *Plugin) Name() string { return Name }

// Supports implements plugin.Deployer.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// Validate implements plugin.Deployer: parameters and workspaces, without running anything.
func (p *Plugin) Validate(req *plugin.Request) error {
	if req == nil {
		return errors.New("terraform plugin: missing request")
	}
	if len(req.Targets) == 0 {
		return errors.New("terraform plugin: no targets: every target is a Terraform state")
	}
	o, err := parseOptions(req.Params)
	if err != nil {
		return err
	}
	seen := map[string]string{}
	for _, t := range req.Targets {
		ws, err := o.workspaceFor(req, t)
		if err != nil {
			return err
		}
		key := ws + "\x00" + expandMap(o.backend, req, t)
		if other, dup := seen[key]; dup {
			return fmt.Errorf("terraform plugin: targets %s and %s use the same state: set workspace or backend.* with {host}", other, t.Host)
		}
		seen[key] = t.Host
	}
	return nil
}

// Deploy implements plugin.Deployer: init, plan and apply on every target, one after the other.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.execute(ctx, req, false)
}

// DryRun implements plugin.DryRunner: init and plan only; the result has the planned changes.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.execute(ctx, req, true)
}

// stateRun is the run of one target.
type stateRun struct {
	p       *Plugin
	req     *plugin.Request
	o       *options
	target  plugin.Target
	binary  string
	module  string
	env     []string
	secrets []string // resolved values, redacted from every error text
}

func (p *Plugin) execute(ctx context.Context, req *plugin.Request, dry bool) (*plugin.Result, error) {
	if err := p.Validate(req); err != nil {
		return nil, permanent(err)
	}
	o, _ := parseOptions(req.Params)
	binary, err := exec.LookPath(p.binary)
	if err != nil {
		return nil, fmt.Errorf("terraform plugin: binary %q not found on the worker: %w", p.binary, err)
	}
	work, err := os.MkdirTemp("", "manent-terraform-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	// Every target gets its own copy of the module: backends and providers may write files next
	// to it (the local backend keeps its workspaces there), and states must never mix.
	modules := make([]string, len(req.Targets))
	for i := range req.Targets {
		src := filepath.Join(work, "src-"+strconv.Itoa(i))
		if err := os.Mkdir(src, 0o700); err != nil {
			return nil, err
		}
		if err := extract(req.Artifact.LocalPath, src); err != nil {
			return nil, permanent(err)
		}
		module, err := moduleRoot(src, o.moduleDir)
		if err != nil {
			return nil, permanent(err)
		}
		if o.lockFile {
			if _, err := os.Stat(filepath.Join(module, ".terraform.lock.hcl")); err != nil {
				return nil, permanent(errors.New("terraform plugin: .terraform.lock.hcl missing from the module: providers must be pinned (require_lock_file: false to allow it)"))
			}
		}
		modules[i] = module
	}
	cliConfig := filepath.Join(work, "cli.tfrc")
	if err := os.WriteFile(cliConfig, []byte(p.cliConfig()), 0o600); err != nil {
		return nil, err
	}

	res := &plugin.Result{Plugin: Name}
	outputs := map[string]map[string]string{} // target -> outputs
	for i, t := range req.Targets {
		home := filepath.Join(work, "home-"+strconv.Itoa(i))
		if err := os.Mkdir(home, 0o700); err != nil {
			return nil, err
		}
		run := &stateRun{p: p, req: req, o: o, target: t, binary: binary, module: modules[i]}
		if err := run.environment(ctx, work, home, cliConfig, i); err != nil {
			return res, err
		}
		tr, out, err := run.do(ctx, dry, filepath.Join(work, "plan-"+strconv.Itoa(i)+".tfplan"))
		res.Targets = append(res.Targets, tr)
		if err != nil {
			return res, err
		}
		if out != nil {
			outputs[t.Host] = out
		}
	}
	if !dry {
		res.Outputs = p.mergeOutputs(ctx, req, outputs)
	}
	return res, nil
}

// cliConfig is the CLI configuration of every run: nothing from the worker's home.
func (p *Plugin) cliConfig() string {
	var b strings.Builder
	if p.pluginCacheDir != "" {
		fmt.Fprintf(&b, "plugin_cache_dir = %q\nplugin_cache_may_break_dependency_lock_file = false\n", p.pluginCacheDir)
	}
	if p.providerMirror != "" {
		fmt.Fprintf(&b, "provider_installation {\n  network_mirror {\n    url = %q\n  }\n}\n", p.providerMirror)
	}
	return b.String()
}

// environment builds the process environment: a few variables of the worker, the plugin's own
// and the variables and secrets of the request. Secrets are resolved now, for this run only.
func (r *stateRun) environment(ctx context.Context, work, home, cliConfig string, i int) error {
	env := []string{"HOME=" + home, "TMPDIR=" + work, "TF_IN_AUTOMATION=1", "TF_INPUT=0",
		"TF_CLI_CONFIG_FILE=" + cliConfig, "TF_DATA_DIR=" + filepath.Join(work, "data-"+strconv.Itoa(i))}
	for _, k := range []string{"PATH", "TZ", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if r.p.pluginCacheDir != "" {
		env = append(env, "TF_PLUGIN_CACHE_DIR="+r.p.pluginCacheDir)
	}
	for _, name := range sortedKeys(r.o.vars) {
		env = append(env, "TF_VAR_"+name+"="+expand(r.o.vars[name], r.req, r.target))
	}
	secret := func(name string) (string, error) {
		if r.req.Secrets == nil {
			return "", fmt.Errorf("terraform plugin: secret %q requested but no secret resolver", name)
		}
		v, err := r.req.Secrets.Secret(ctx, name)
		if err != nil {
			return "", fmt.Errorf("terraform plugin: secret %q: %w", name, err)
		}
		if v != "" {
			r.secrets = append(r.secrets, v)
		}
		return v, nil
	}
	for _, name := range sortedKeys(r.o.secretVars) {
		v, err := secret(r.o.secretVars[name])
		if err != nil {
			return err
		}
		env = append(env, "TF_VAR_"+name+"="+v)
	}
	for _, name := range sortedKeys(r.o.secretEnv) {
		v, err := secret(r.o.secretEnv[name])
		if err != nil {
			return err
		}
		env = append(env, name+"="+v)
	}
	r.env = env
	return nil
}

// do runs init, workspace, plan and, unless dry, apply and output on one target.
func (r *stateRun) do(ctx context.Context, dry bool, planFile string) (plugin.TargetResult, map[string]string, error) {
	t := r.target
	tr := plugin.TargetResult{Host: t.Host, Status: plugin.StatusFailed, Details: map[string]string{}}
	ctx, cancel := context.WithTimeout(ctx, r.o.timeout)
	defer cancel()
	log := r.p.logger.With("target", t.Host, "deployment", r.req.DeploymentID)

	init := []string{"init", "-input=false", "-no-color"}
	if r.o.lockFile {
		init = append(init, "-lockfile=readonly")
	}
	for _, k := range sortedKeys(r.o.backend) {
		init = append(init, "-backend-config="+k+"="+expand(r.o.backend[k], r.req, t))
	}
	if _, err := r.cmd(ctx, "init", init...); err != nil {
		return tr, nil, err
	}
	ws, _ := r.o.workspaceFor(r.req, t)
	switch {
	case ws != "" && dry:
		// A dry-run must leave the backend as it was: a missing workspace is created only for
		// the plan and deleted afterwards (it is still empty, a plan writes no state).
		created, err := r.dryRunWorkspace(ctx, ws)
		if err != nil {
			return tr, nil, err
		}
		if created {
			defer r.dropWorkspace(ctx, ws)
		}
		tr.Details["workspace"] = ws
	case ws != "":
		if _, err := r.cmd(ctx, "workspace select", "workspace", "select", "-or-create=true", "-no-color", ws); err != nil {
			return tr, nil, err
		}
		tr.Details["workspace"] = ws
	}

	plan := []string{"plan", "-input=false", "-no-color", "-detailed-exitcode", "-out=" + planFile}
	if r.o.parallelism > 0 {
		plan = append(plan, "-parallelism="+strconv.Itoa(r.o.parallelism))
	}
	_, stderr, exit, err := r.p.run.Run(ctx, r.module, r.env, r.binary, plan...)
	if err != nil {
		return tr, nil, fmt.Errorf("terraform plugin: plan on %s: %w", t.Host, err)
	}
	if exit != 0 && exit != 2 {
		return tr, nil, r.failure("plan", exit, stderr)
	}
	changes := exit == 2
	show, err := r.cmd(ctx, "show", "show", "-json", "-no-color", planFile)
	if err != nil {
		return tr, nil, err
	}
	sum, err := summarize(show)
	if err != nil {
		return tr, nil, fmt.Errorf("terraform plugin: read the plan of %s: %w", t.Host, err)
	}
	sum.details(tr.Details)
	tr.Details["changes"] = strconv.FormatBool(changes)
	log.InfoContext(ctx, "terraform plan", "add", sum.add, "change", sum.change, "destroy", sum.destroy, "dry_run", dry)
	if sum.destroy > r.o.maxDestroy {
		return tr, nil, permanent(fmt.Errorf("terraform plugin: the plan of %s destroys %d resources, more than max_destroy (%d): %s",
			t.Host, sum.destroy, r.o.maxDestroy, strings.Join(sum.destroyed, ", ")))
	}
	if dry {
		tr.Status = plugin.StatusReady
		return tr, nil, nil
	}
	if changes {
		apply := []string{"apply", "-input=false", "-no-color"}
		if r.o.parallelism > 0 {
			apply = append(apply, "-parallelism="+strconv.Itoa(r.o.parallelism))
		}
		if _, err := r.cmd(ctx, "apply", append(apply, planFile)...); err != nil {
			return tr, nil, err
		}
		log.InfoContext(ctx, "terraform apply completed")
	}
	raw, err := r.cmd(ctx, "output", "output", "-json", "-no-color")
	if err != nil {
		return tr, nil, err
	}
	outputs, err := parseOutputs(raw)
	if err != nil {
		return tr, nil, fmt.Errorf("terraform plugin: read the outputs of %s: %w", t.Host, err)
	}
	tr.Status = plugin.StatusDeployed
	return tr, outputs, nil
}

// dryRunWorkspace selects ws if it exists, else creates it and reports that it did.
func (r *stateRun) dryRunWorkspace(ctx context.Context, ws string) (bool, error) {
	out, err := r.cmd(ctx, "workspace list", "workspace", "list", "-no-color")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*")) == ws {
			_, err := r.cmd(ctx, "workspace select", "workspace", "select", "-no-color", ws)
			return false, err
		}
	}
	if _, err := r.cmd(ctx, "workspace new", "workspace", "new", "-no-color", ws); err != nil {
		return false, err
	}
	return true, nil
}

// dropWorkspace deletes the empty workspace created by a dry-run; a failure is only logged.
func (r *stateRun) dropWorkspace(ctx context.Context, ws string) {
	_, err := r.cmd(ctx, "workspace select", "workspace", "select", "-no-color", "default")
	if err == nil {
		_, err = r.cmd(ctx, "workspace delete", "workspace", "delete", "-no-color", ws)
	}
	if err != nil {
		r.p.logger.WarnContext(ctx, "terraform dry-run: workspace created for the plan not deleted", "target", r.target.Host, "workspace", ws, "error", err)
	}
}

// cmd runs a command that must exit 0 and returns its stdout.
func (r *stateRun) cmd(ctx context.Context, what string, args ...string) ([]byte, error) {
	stdout, stderr, exit, err := r.p.run.Run(ctx, r.module, r.env, r.binary, args...)
	if err != nil {
		return nil, fmt.Errorf("terraform plugin: %s on %s: %w", what, r.target.Host, err)
	}
	if exit != 0 {
		return nil, r.failure(what, exit, stderr)
	}
	return stdout, nil
}

// failure is the error of a command, with the end of its stderr and the secrets redacted.
func (r *stateRun) failure(what string, exit int, stderr []byte) error {
	text := strings.TrimSpace(string(stderr))
	for _, s := range r.secrets {
		text = strings.ReplaceAll(text, s, "***")
	}
	if len(text) > maxErrorText {
		text = "…" + text[len(text)-maxErrorText:]
	}
	return fmt.Errorf("terraform plugin: %s on %s failed (exit %d): %s", what, r.target.Host, exit, text)
}

// summary counts the planned changes; the addresses are reported, the values never.
type summary struct {
	add, change, destroy int
	addresses, destroyed []string
}

func summarize(planJSON []byte) (summary, error) {
	var plan struct {
		ResourceChanges []struct {
			Address string `json:"address"`
			Change  struct {
				Actions []string `json:"actions"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	var s summary
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return s, err
	}
	for _, rc := range plan.ResourceChanges {
		touched := false
		for _, a := range rc.Change.Actions {
			switch a {
			case "create":
				s.add++
				touched = true
			case "update":
				s.change++
				touched = true
			case "delete":
				s.destroy++
				s.destroyed = append(s.destroyed, rc.Address)
				touched = true
			}
		}
		if touched {
			s.addresses = append(s.addresses, rc.Address)
		}
	}
	return s, nil
}

func (s summary) details(d map[string]string) {
	d["add"], d["change"], d["destroy"] = strconv.Itoa(s.add), strconv.Itoa(s.change), strconv.Itoa(s.destroy)
	if len(s.addresses) > 0 {
		list := s.addresses
		if len(list) > maxAddresses {
			list = append(slices.Clone(list[:maxAddresses]), fmt.Sprintf("… %d more", len(s.addresses)-maxAddresses))
		}
		d["resources"] = strings.Join(list, ", ")
	}
}

// parseOutputs reads `output -json`: sensitive outputs are left out, strings stay as they are,
// any other value becomes compact JSON (a list of hosts is a valid "targets" export).
func parseOutputs(raw []byte) (map[string]string, error) {
	var outs map[string]struct {
		Sensitive bool            `json:"sensitive"`
		Value     json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &outs); err != nil {
		return nil, err
	}
	res := map[string]string{}
	for name, o := range outs {
		if o.Sensitive {
			continue
		}
		var s string
		if err := json.Unmarshal(o.Value, &s); err == nil {
			res[name] = s
			continue
		}
		var v any
		if err := json.Unmarshal(o.Value, &v); err != nil {
			return nil, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		res[name] = string(b)
	}
	return res, nil
}

// mergeOutputs builds the plugin outputs. With one target the keys are the output names; with
// several, <host>.<name> for each target, plus <name> when every target has the same value.
// Keys or values over manent's limits are left out with a warning: the apply already happened.
func (p *Plugin) mergeOutputs(ctx context.Context, req *plugin.Request, outputs map[string]map[string]string) map[string]string {
	res := map[string]string{}
	add := func(k, v string) {
		if !outputKeyRe.MatchString(k) {
			p.logger.WarnContext(ctx, "terraform output skipped: invalid key", "key", k)
			return
		}
		res[k] = v
	}
	if len(req.Targets) == 1 {
		for k, v := range outputs[req.Targets[0].Host] {
			add(k, v)
		}
	} else {
		common := map[string]string{}
		count := map[string]int{}
		for _, t := range req.Targets {
			for k, v := range outputs[t.Host] {
				add(t.Host+"."+k, v)
				if prev, ok := common[k]; !ok || prev == v {
					common[k] = v
					count[k]++
				}
			}
		}
		for k, v := range common {
			if count[k] == len(req.Targets) {
				add(k, v)
			}
		}
	}
	keys := sortedKeys(res)
	size := 0
	for i, k := range keys {
		size += len(k) + len(res[k])
		if i >= plugin.MaxOutputs || size > plugin.MaxOutputBytes {
			p.logger.WarnContext(ctx, "terraform output skipped: over manent's limits", "key", k)
			delete(res, k)
		}
	}
	if len(res) == 0 {
		return nil
	}
	return res
}

func expandMap(m map[string]string, req *plugin.Request, t plugin.Target) string {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(k + "=" + expand(m[k], req, t) + "\x00")
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// permanent marks the errors a retry cannot fix (invalid parameters, a plan over max_destroy).
func permanent(err error) error { return plugin.Permanent(err) }

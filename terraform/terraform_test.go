package terraform

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// call is one run of the binary seen by the fake runner.
type call struct {
	dir  string
	env  []string
	args []string
}

func (c call) envValue(k string) (string, bool) {
	for _, e := range c.env {
		if v, ok := strings.CutPrefix(e, k+"="); ok {
			return v, true
		}
	}
	return "", false
}

// fakeRunner answers like tofu: plan exits with planExit, show prints plan, output prints outputs.
type fakeRunner struct {
	calls    []call
	planExit int
	plan     string
	outputs  map[string]string // target workspace -> output -json
	fail     map[string]string // command -> stderr of a failure
	list     string            // output of workspace list
}

func (f *fakeRunner) Run(_ context.Context, dir string, env []string, _ string, args ...string) ([]byte, []byte, int, error) {
	c := call{dir: dir, env: env, args: args}
	f.calls = append(f.calls, c)
	if msg, ok := f.fail[args[0]]; ok {
		return nil, []byte(msg), 1, nil
	}
	switch args[0] {
	case "workspace":
		if args[1] == "list" {
			return []byte(f.list), nil, 0, nil
		}
	case "plan":
		return nil, nil, f.planExit, nil
	case "show":
		return []byte(f.plan), nil, 0, nil
	case "output":
		data, _ := c.envValue("TF_DATA_DIR")
		if out, ok := f.outputs[data]; ok {
			return []byte(out), nil, 0, nil
		}
		return []byte("{}"), nil, 0, nil
	}
	return nil, nil, 0, nil
}

func (f *fakeRunner) commands() []string {
	var cmds []string
	for _, c := range f.calls {
		cmds = append(cmds, c.args[0])
	}
	return cmds
}

type secrets map[string]string

func (s secrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", errors.New("unknown secret")
	}
	return v, nil
}

// archive writes a .tgz with the given files; a value "->target" is a symlink.
func archive(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		body := files[name]
		if target, ok := strings.CutPrefix(body, "->"); ok {
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
			continue
		}
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "module.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var module = map[string]string{"network/main.tf": `resource "terraform_data" "vm" {}`, "network/.terraform.lock.hcl": "# lock"}

func newPlugin(t *testing.T, run Runner) *Plugin {
	t.Helper()
	d, err := Factory(map[string]any{"binary": "/bin/sh", "plugin_cache_dir": "/var/cache/manent/tf"}, discard)
	if err != nil {
		t.Fatal(err)
	}
	p := d.(*Plugin)
	p.run = run
	return p
}

func request(t *testing.T, files map[string]string, params map[string]string, hosts ...string) *plugin.Request {
	t.Helper()
	req := &plugin.Request{DeploymentID: "d1", Tenant: "untenanted", Application: "infra", Environment: "prod",
		Artifact: plugin.Artifact{Type: "terraform", Name: "network", Version: "1.0.0", LocalPath: archive(t, files)},
		Params:   params, Secrets: secrets{"db": "s3cr3t-db", "aws": "s3cr3t-aws"}}
	for _, h := range hosts {
		req.Targets = append(req.Targets, plugin.Target{Host: h})
	}
	return req
}

const planWithChanges = `{"resource_changes":[
 {"address":"terraform_data.vm[0]","change":{"actions":["create"]}},
 {"address":"terraform_data.dns","change":{"actions":["update"]}},
 {"address":"terraform_data.old","change":{"actions":["no-op"]}}]}`

func TestDeploy(t *testing.T) {
	t.Setenv("LEAK_ME", "worker-secret")
	run := &fakeRunner{planExit: 2, plan: planWithChanges, outputs: map[string]string{}}
	p := newPlugin(t, run)
	params := map[string]string{"workspace": "{host}", "backend.bucket": "acme-tfstate", "backend.key": "net/{environment}/{host}.tfstate",
		"var.cidr": "10.20.0.0/16", "var.name": "{application}-{host}", "secret_var.db_password": "db", "env.AWS_SECRET_ACCESS_KEY": "aws",
		"max_destroy": "0", "parallelism": "5"}
	req := request(t, module, params, "prod-eu", "prod-us")
	// Outputs per target, keyed by the TF_DATA_DIR of each run.
	run.outputs = nil
	p.run = &outputsByWorkspace{fakeRunner: run, outputs: map[string]string{
		"prod-eu": `{"vm_hosts":{"sensitive":false,"value":["10.0.1.4"]},"region":{"sensitive":false,"value":"shared"},"db_pass":{"sensitive":true,"value":"x"}}`,
		"prod-us": `{"vm_hosts":{"sensitive":false,"value":["10.1.1.4"]},"region":{"sensitive":false,"value":"shared"}}`,
	}}

	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"init", "workspace", "plan", "show", "apply", "output", "init", "workspace", "plan", "show", "apply", "output"}
	if got := run.commands(); !slices.Equal(got, want) {
		t.Fatalf("commands: %v", got)
	}
	init := run.calls[0]
	if !slices.Contains(init.args, "-lockfile=readonly") || !slices.Contains(init.args, "-backend-config=key=net/prod/prod-eu.tfstate") ||
		!slices.Contains(init.args, "-backend-config=bucket=acme-tfstate") {
		t.Errorf("init: %v", init.args)
	}
	if ws := run.calls[1].args; ws[len(ws)-1] != "prod-eu" || !slices.Contains(ws, "-or-create=true") {
		t.Errorf("workspace: %v", ws)
	}
	if plan := run.calls[2].args; !slices.Contains(plan, "-parallelism=5") || !slices.Contains(plan, "-detailed-exitcode") {
		t.Errorf("plan: %v", plan)
	}
	for k, v := range map[string]string{"TF_VAR_cidr": "10.20.0.0/16", "TF_VAR_name": "infra-prod-eu", "TF_VAR_db_password": "s3cr3t-db",
		"AWS_SECRET_ACCESS_KEY": "s3cr3t-aws", "TF_IN_AUTOMATION": "1", "TF_PLUGIN_CACHE_DIR": "/var/cache/manent/tf"} {
		if got, _ := init.envValue(k); got != v {
			t.Errorf("env %s = %q, want %q", k, got, v)
		}
	}
	if _, ok := init.envValue("LEAK_ME"); ok {
		t.Error("the worker environment leaked into the process")
	}
	home, _ := init.envValue("HOME")
	cfg, _ := init.envValue("TF_CLI_CONFIG_FILE")
	if !strings.Contains(home, "manent-terraform-") || !strings.HasSuffix(cfg, "cli.tfrc") {
		t.Errorf("HOME %q, TF_CLI_CONFIG_FILE %q", home, cfg)
	}
	if !strings.HasSuffix(init.dir, "/network") {
		t.Errorf("the module root was not found in the single top-level directory: %s", init.dir)
	}
	d0, _ := run.calls[0].envValue("TF_DATA_DIR")
	d1, _ := run.calls[6].envValue("TF_DATA_DIR")
	if d0 == d1 {
		t.Error("the two states share TF_DATA_DIR")
	}
	if _, err := os.Stat(filepath.Dir(home)); !os.IsNotExist(err) {
		t.Error("the working directory was not removed")
	}

	tr := res.Targets[0]
	if tr.Status != plugin.StatusDeployed || tr.Details["add"] != "1" || tr.Details["change"] != "1" || tr.Details["destroy"] != "0" ||
		tr.Details["workspace"] != "prod-eu" || tr.Details["changes"] != "true" {
		t.Errorf("target result: %+v", tr)
	}
	wantOut := map[string]string{"prod-eu.vm_hosts": `["10.0.1.4"]`, "prod-us.vm_hosts": `["10.1.1.4"]`,
		"prod-eu.region": "shared", "prod-us.region": "shared", "region": "shared"}
	if len(res.Outputs) != len(wantOut) {
		t.Errorf("outputs: %v", res.Outputs)
	}
	for k, v := range wantOut {
		if res.Outputs[k] != v {
			t.Errorf("output %s = %q, want %q (%v)", k, res.Outputs[k], v, res.Outputs)
		}
	}
}

// outputsByWorkspace answers `output -json` with the outputs of the selected workspace.
type outputsByWorkspace struct {
	*fakeRunner
	outputs map[string]string
	ws      string
}

func (o *outputsByWorkspace) Run(ctx context.Context, dir string, env []string, bin string, args ...string) ([]byte, []byte, int, error) {
	if args[0] == "workspace" {
		o.ws = args[len(args)-1]
	}
	if args[0] == "output" {
		o.calls = append(o.calls, call{dir: dir, env: env, args: args})
		return []byte(o.outputs[o.ws]), nil, 0, nil
	}
	return o.fakeRunner.Run(ctx, dir, env, bin, args...)
}

func TestSingleTargetOutputs(t *testing.T) {
	run := &fakeRunner{planExit: 0, plan: `{}`, outputs: map[string]string{}}
	p := newPlugin(t, &outputsByWorkspace{fakeRunner: run, outputs: map[string]string{"": `{"fqdn":{"value":"db.example.com"},"ports":{"value":[5432]}}`}})
	res, err := p.Deploy(context.Background(), request(t, module, nil, "prod"))
	if err != nil {
		t.Fatal(err)
	}
	// No changes: no apply, but the outputs are read.
	if got := run.commands(); !slices.Equal(got, []string{"init", "plan", "show", "output"}) {
		t.Errorf("commands: %v", got)
	}
	if res.Outputs["fqdn"] != "db.example.com" || res.Outputs["ports"] != "[5432]" || res.Targets[0].Details["changes"] != "false" {
		t.Errorf("result: %+v", res)
	}
}

func TestDryRun(t *testing.T) {
	run := &fakeRunner{planExit: 2, plan: planWithChanges}
	p := newPlugin(t, run)
	res, err := p.DryRun(context.Background(), request(t, module, nil, "prod"))
	if err != nil {
		t.Fatal(err)
	}
	if got := run.commands(); !slices.Equal(got, []string{"init", "plan", "show"}) {
		t.Errorf("a dry-run must not apply: %v", got)
	}
	tr := res.Targets[0]
	if tr.Status != plugin.StatusReady || tr.Details["resources"] != "terraform_data.vm[0], terraform_data.dns" || res.Outputs != nil {
		t.Errorf("dry-run result: %+v", res)
	}
}

func TestDryRunWorkspace(t *testing.T) {
	// A missing workspace is created for the plan and deleted afterwards.
	run := &fakeRunner{planExit: 2, plan: planWithChanges, list: "* default\n"}
	p := newPlugin(t, run)
	if _, err := p.DryRun(context.Background(), request(t, module, map[string]string{"workspace": "{host}"}, "prod-eu")); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range run.calls {
		got = append(got, strings.Join(c.args[:min(len(c.args), 2)], " "))
	}
	want := []string{"init -input=false", "workspace list", "workspace new", "plan -input=false", "show -json", "workspace select", "workspace delete"}
	if !slices.Equal(got, want) {
		t.Fatalf("commands: %v", got)
	}
	if last := run.calls[len(run.calls)-1].args; last[len(last)-1] != "prod-eu" || run.calls[5].args[len(run.calls[5].args)-1] != "default" {
		t.Errorf("workspace cleanup: %v", run.calls[5:])
	}
	// An existing workspace is only selected, never created or deleted.
	run = &fakeRunner{planExit: 0, plan: `{}`, list: "  default\n* prod-eu\n"}
	p = newPlugin(t, run)
	if _, err := p.DryRun(context.Background(), request(t, module, map[string]string{"workspace": "{host}"}, "prod-eu")); err != nil {
		t.Fatal(err)
	}
	for _, c := range run.calls {
		if c.args[0] == "workspace" && c.args[1] != "list" && c.args[1] != "select" {
			t.Errorf("unexpected workspace command: %v", c.args)
		}
	}
}

func TestMaxDestroy(t *testing.T) {
	plan := `{"resource_changes":[{"address":"aws_db_instance.main","change":{"actions":["delete","create"]}}]}`
	run := &fakeRunner{planExit: 2, plan: plan}
	p := newPlugin(t, run)
	_, err := p.Deploy(context.Background(), request(t, module, nil, "prod"))
	if !errors.Is(err, plugin.ErrPermanent) || !strings.Contains(err.Error(), "aws_db_instance.main") || !strings.Contains(err.Error(), "max_destroy (0)") {
		t.Fatalf("expected a permanent max_destroy error: %v", err)
	}
	if slices.Contains(run.commands(), "apply") {
		t.Error("a plan over max_destroy was applied")
	}
	// With max_destroy 1 the replacement goes through.
	run = &fakeRunner{planExit: 2, plan: plan, outputs: map[string]string{}}
	p = newPlugin(t, run)
	if _, err := p.Deploy(context.Background(), request(t, module, map[string]string{"max_destroy": "1"}, "prod")); err != nil {
		t.Fatal(err)
	}
}

func TestFailureRedactsSecrets(t *testing.T) {
	run := &fakeRunner{fail: map[string]string{"init": "Error: invalid credentials s3cr3t-aws for bucket"}}
	p := newPlugin(t, run)
	_, err := p.Deploy(context.Background(), request(t, module, map[string]string{"env.AWS_SECRET_ACCESS_KEY": "aws"}, "prod"))
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "invalid credentials *** for bucket") {
		t.Fatalf("error: %v", err)
	}
	if errors.Is(err, plugin.ErrPermanent) {
		t.Error("a failed init can be transient: it must not be permanent")
	}
}

func TestMissingSecret(t *testing.T) {
	p := newPlugin(t, &fakeRunner{})
	_, err := p.Deploy(context.Background(), request(t, module, map[string]string{"secret_var.token": "missing"}, "prod"))
	if err == nil || !strings.Contains(err.Error(), `secret "missing"`) {
		t.Fatalf("error: %v", err)
	}
}

func TestValidate(t *testing.T) {
	p := newPlugin(t, &fakeRunner{})
	for name, tc := range map[string]struct {
		params map[string]string
		hosts  []string
		want   string
	}{
		"unknown":        {map[string]string{"workspce": "x"}, []string{"a"}, `unknown parameter "workspce"`},
		"same state":     {nil, []string{"a", "b"}, "use the same state"},
		"bad workspace":  {map[string]string{"workspace": "{host}"}, []string{"bad host"}, "invalid workspace"},
		"reserved env":   {map[string]string{"env.TF_DATA_DIR": "x"}, []string{"a"}, "is set by the plugin"},
		"duplicate var":  {map[string]string{"var.a": "1", "secret_var.a": "s"}, []string{"a"}, "both by var.a"},
		"max_destroy":    {map[string]string{"max_destroy": "-1"}, []string{"a"}, "non-negative"},
		"module escapes": {map[string]string{"module_dir": "../x"}, []string{"a"}, "relative path"},
		"no targets":     {nil, nil, "no targets"},
	} {
		req := &plugin.Request{Params: tc.params}
		for _, h := range tc.hosts {
			req.Targets = append(req.Targets, plugin.Target{Host: h})
		}
		if err := p.Validate(req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := &plugin.Request{Params: map[string]string{"workspace": "{host}"}, Targets: []plugin.Target{{Host: "eu-1"}, {Host: "us-1"}}}
	if err := p.Validate(ok); err != nil {
		t.Errorf("valid request refused: %v", err)
	}
}

func TestArtifactChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		files  map[string]string
		params map[string]string
		want   string
	}{
		"traversal":  {map[string]string{"../evil.tf": "x"}, nil, "unsafe path"},
		"symlink":    {map[string]string{"main.tf": "x", "link": "->/etc/passwd"}, nil, "link or a special file"},
		"no module":  {map[string]string{"README.md": "x"}, nil, "no .tf or .tf.json"},
		"no lock":    {map[string]string{"main.tf": "x"}, nil, ".terraform.lock.hcl missing"},
		"module_dir": {map[string]string{"stacks/net/main.tf": "x"}, map[string]string{"module_dir": "stacks/db"}, "no .tf or .tf.json"},
	} {
		run := &fakeRunner{}
		p := newPlugin(t, run)
		_, err := p.Deploy(context.Background(), request(t, tc.files, tc.params, "prod"))
		if !errors.Is(err, plugin.ErrPermanent) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if len(run.calls) > 0 {
			t.Errorf("%s: the binary ran on an invalid artifact", name)
		}
	}
	// require_lock_file false and module_dir.
	run := &fakeRunner{planExit: 0, plan: `{}`, outputs: map[string]string{}}
	p := newPlugin(t, run)
	if _, err := p.Deploy(context.Background(), request(t, map[string]string{"stacks/net/main.tf": "x"},
		map[string]string{"module_dir": "stacks/net", "require_lock_file": "false"}, "prod")); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(run.calls[0].args, "-lockfile=readonly") || !strings.HasSuffix(run.calls[0].dir, "stacks/net") {
		t.Errorf("init: %+v", run.calls[0])
	}
}

func TestBinaryNotFound(t *testing.T) {
	d, _ := Factory(map[string]any{"binary": "tofu-not-installed"}, discard)
	_, err := d.Deploy(context.Background(), request(t, module, nil, "prod"))
	if err == nil || !strings.Contains(err.Error(), `binary "tofu-not-installed" not found`) {
		t.Fatalf("error: %v", err)
	}
}

func TestFactory(t *testing.T) {
	d, err := Factory(map[string]any{"artifact_types": []any{"infrastructure"}, "external": map[string]any{"command": "x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != Name || !d.Supports("infrastructure") || d.Supports("terraform") {
		t.Errorf("artifact types not applied")
	}
	for _, cfg := range []map[string]any{
		{"artifact_types": "terraform"},
		{"plugin_cache_dir": "relative"},
		{"provider_mirror": "http://mirror"},
		{"binary": 3},
	} {
		if _, err := Factory(cfg, nil); err == nil {
			t.Errorf("invalid configuration accepted: %v", cfg)
		}
	}
	p, _ := Factory(map[string]any{"plugin_cache_dir": "/cache", "provider_mirror": "https://mirror.example.com/"}, nil)
	cfg := p.(*Plugin).cliConfig()
	if !strings.Contains(cfg, `plugin_cache_dir = "/cache"`) || !strings.Contains(cfg, `url = "https://mirror.example.com/"`) {
		t.Errorf("CLI configuration:\n%s", cfg)
	}
}

func TestExecRunner(t *testing.T) {
	t.Setenv("LEAK_ME", "worker-secret")
	out, _, exit, err := execRunner{}.Run(context.Background(), t.TempDir(), []string{"ONLY=this"}, "/usr/bin/env")
	if err != nil || exit != 0 || strings.TrimSpace(string(out)) != "ONLY=this" {
		t.Fatalf("env: %q %d %v", out, exit, err)
	}
	_, stderr, exit, err := execRunner{}.Run(context.Background(), t.TempDir(), nil, "/bin/sh", "-c", "echo boom >&2; exit 3")
	if err != nil || exit != 3 || strings.TrimSpace(string(stderr)) != "boom" {
		t.Fatalf("exit: %q %d %v", stderr, exit, err)
	}
}

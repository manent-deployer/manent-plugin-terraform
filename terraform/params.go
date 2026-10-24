package terraform

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Parameter names and prefixes accepted in the environment configuration and the descriptor.
const (
	paramWorkspace   = "workspace"
	paramModuleDir   = "module_dir"
	paramMaxDestroy  = "max_destroy"
	paramTimeout     = "timeout"
	paramParallelism = "parallelism"
	paramLockFile    = "require_lock_file"
	prefixBackend    = "backend."
	prefixVar        = "var."
	prefixSecretVar  = "secret_var."
	prefixEnv        = "env."
)

var (
	// identRe are Terraform variable names and environment variable names.
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// workspaceRe are the workspace names manent accepts (they also end up in logs and outputs).
	workspaceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,89}$`)
	// placeholderRe are the placeholders expanded in workspace, backend and var values.
	placeholderRe = regexp.MustCompile(`\{(host|application|artifact|environment|tenant)\}`)
	// reservedEnv are environment variables the plugin sets itself: env.* cannot override them.
	reservedEnv = []string{"PATH", "HOME", "TMPDIR", "TZ", "LANG", "TF_IN_AUTOMATION", "TF_INPUT",
		"TF_CLI_CONFIG_FILE", "TF_DATA_DIR", "TF_PLUGIN_CACHE_DIR", "TF_WORKSPACE", "TF_LOG", "TF_LOG_PATH",
		"TF_CLI_ARGS"}
)

// options are the parameters of a request, checked and typed.
type options struct {
	workspace   string // may contain placeholders
	moduleDir   string
	maxDestroy  int
	timeout     time.Duration
	parallelism int
	lockFile    bool
	backend     map[string]string // -backend-config key=value, values may contain placeholders
	vars        map[string]string // TF_VAR_<name>, values may contain placeholders
	secretVars  map[string]string // TF_VAR_<name> = value of the named secret
	secretEnv   map[string]string // <NAME> = value of the named secret
}

// parseOptions checks every parameter: an unknown key is an error, so a typo never goes unnoticed.
func parseOptions(params map[string]string) (*options, error) {
	o := &options{moduleDir: ".", timeout: time.Hour, lockFile: true,
		backend: map[string]string{}, vars: map[string]string{}, secretVars: map[string]string{}, secretEnv: map[string]string{}}
	var errs []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		v := params[k]
		switch {
		case k == paramWorkspace:
			o.workspace = v
		case k == paramModuleDir:
			clean := path.Clean(v)
			if v == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				errs = append(errs, fmt.Sprintf("%s %q must be a relative path inside the artifact", k, v))
			}
			o.moduleDir = clean
		case k == paramMaxDestroy:
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				errs = append(errs, fmt.Sprintf("%s %q must be a non-negative integer", k, v))
			}
			o.maxDestroy = n
		case k == paramTimeout:
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Sprintf("%s %q must be a positive duration (e.g. 30m)", k, v))
			}
			o.timeout = d
		case k == paramParallelism:
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 256 {
				errs = append(errs, fmt.Sprintf("%s %q must be an integer between 1 and 256", k, v))
			}
			o.parallelism = n
		case k == paramLockFile:
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s %q must be true or false", k, v))
			}
			o.lockFile = b
		case strings.HasPrefix(k, prefixBackend):
			name := strings.TrimPrefix(k, prefixBackend)
			if !identRe.MatchString(name) {
				errs = append(errs, fmt.Sprintf("invalid backend setting %q", k))
			}
			o.backend[name] = v
		case strings.HasPrefix(k, prefixVar):
			name := strings.TrimPrefix(k, prefixVar)
			if !identRe.MatchString(name) {
				errs = append(errs, fmt.Sprintf("invalid variable name %q", k))
			}
			o.vars[name] = v
		case strings.HasPrefix(k, prefixSecretVar):
			name := strings.TrimPrefix(k, prefixSecretVar)
			if !identRe.MatchString(name) || v == "" {
				errs = append(errs, fmt.Sprintf("%s: invalid variable name or empty secret name", k))
			}
			o.secretVars[name] = v
		case strings.HasPrefix(k, prefixEnv):
			name := strings.TrimPrefix(k, prefixEnv)
			if !identRe.MatchString(name) || v == "" {
				errs = append(errs, fmt.Sprintf("%s: invalid variable name or empty secret name", k))
			} else if slices.Contains(reservedEnv, strings.ToUpper(name)) || strings.HasPrefix(strings.ToUpper(name), "TF_VAR_") {
				errs = append(errs, fmt.Sprintf("%s: %s is set by the plugin", k, name))
			}
			o.secretEnv[name] = v
		default:
			errs = append(errs, fmt.Sprintf("unknown parameter %q", k))
		}
	}
	for name := range o.secretVars {
		if _, dup := o.vars[name]; dup {
			errs = append(errs, fmt.Sprintf("variable %q is set both by var.%s and secret_var.%s", name, name, name))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("terraform plugin: %s", strings.Join(errs, "; "))
	}
	return o, nil
}

// expand replaces the placeholders with the values of the request and the target.
func expand(s string, req *plugin.Request, t plugin.Target) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		switch m {
		case "{host}":
			return t.Host
		case "{application}":
			return req.Application
		case "{artifact}":
			return req.Artifact.Name
		case "{environment}":
			return req.Environment
		case "{tenant}":
			return req.Tenant
		}
		return m
	})
}

// workspaceFor returns the workspace of a target, checked; "" means the default workspace.
func (o *options) workspaceFor(req *plugin.Request, t plugin.Target) (string, error) {
	if o.workspace == "" {
		return "", nil
	}
	ws := expand(o.workspace, req, t)
	if !workspaceRe.MatchString(ws) {
		return "", fmt.Errorf("terraform plugin: invalid workspace %q for target %s", ws, t.Host)
	}
	return ws, nil
}

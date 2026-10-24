// Package terraform implements the Terraform deploy plugin of manent. The implementation follows the
// design document "Plugin Terraform e Ansible"; until then Deploy refuses every request.
package terraform

import (
	"context"
	"errors"
	"log/slog"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Name is the plugin name and the artifact type it handles.
const Name = "terraform"

// ErrNotImplemented is returned by Deploy until the plugin is implemented.
var ErrNotImplemented = errors.New("terraform plugin: deploy not implemented yet")

// Plugin is the Terraform deployer.
type Plugin struct {
	logger *slog.Logger
}

// Factory builds the plugin; it is the same for an embedded and an external plugin.
func Factory(_ map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return &Plugin{logger: logger}, nil
}

// Name implements plugin.Deployer.
func (p *Plugin) Name() string { return Name }

// Supports implements plugin.Deployer: only artifacts of type "terraform".
func (p *Plugin) Supports(artifactType string) bool { return artifactType == Name }

// Validate implements plugin.Deployer.
func (p *Plugin) Validate(req *plugin.Request) error {
	if req == nil {
		return errors.New("terraform plugin: missing request")
	}
	if len(req.Targets) == 0 {
		return errors.New("terraform plugin: no targets")
	}
	return nil
}

// Deploy implements plugin.Deployer.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return nil, ErrNotImplemented
}

package terraform

import (
	"context"
	"errors"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

func TestFactory(t *testing.T) {
	d, err := Factory(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != Name || !d.Supports(Name) || d.Supports("application") {
		t.Errorf("unexpected plugin: %s", d.Name())
	}
	if err := d.Validate(&plugin.Request{}); err == nil {
		t.Error("request without targets accepted")
	}
	if _, err := d.Deploy(context.Background(), &plugin.Request{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("deploy: %v", err)
	}
}

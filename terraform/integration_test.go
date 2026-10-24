package terraform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// TestRealBinary runs the plugin against a real tofu or terraform binary (MANENT_TEST_TOFU, an
// absolute path), with terraform_data resources and a local backend: nothing is downloaded.
func TestRealBinary(t *testing.T) {
	bin := os.Getenv("MANENT_TEST_TOFU")
	if bin == "" {
		t.Skip("MANENT_TEST_TOFU not set")
	}
	d, err := Factory(map[string]any{"binary": bin}, discard)
	if err != nil {
		t.Fatal(err)
	}
	states := t.TempDir()
	v1 := map[string]string{
		"main.tf": `terraform {
  backend "local" {}
}
variable "cidr" { type = string }
variable "token" {
  type      = string
  sensitive = true
}
resource "terraform_data" "vm" {
  count = 2
  input = "${var.cidr}-${count.index}"
}
output "vm_hosts" { value = [for i in range(2) : "10.0.0.${i + 4}"] }
output "cidr" { value = var.cidr }
output "token" {
  value     = var.token
  sensitive = true
}
`}
	params := map[string]string{"backend.path": filepath.Join(states, "terraform.tfstate"), "backend.workspace_dir": filepath.Join(states, "terraform.tfstate.d"),
		"workspace": "{host}",
		"var.cidr":  "10.20.0.0/16", "secret_var.token": "db", "require_lock_file": "false"}
	req := request(t, v1, params, "eu", "us")
	ctx := context.Background()

	dry, err := d.(plugin.DryRunner).DryRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Targets[0].Details["add"] != "2" || dry.Targets[0].Status != plugin.StatusReady {
		t.Fatalf("dry-run: %+v", dry.Targets[0])
	}
	// The local backend keeps workspaces in terraform.tfstate.d: the dry-run leaves none behind.
	if entries, _ := os.ReadDir(filepath.Join(states, "terraform.tfstate.d")); len(entries) > 0 {
		t.Fatalf("the dry-run left workspaces: %v", entries)
	}

	res, err := d.Deploy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["vm_hosts"] != `["10.0.0.4","10.0.0.5"]` || res.Outputs["eu.cidr"] != "10.20.0.0/16" || res.Outputs["token"] != "" {
		t.Errorf("outputs: %v", res.Outputs)
	}
	for _, h := range []string{"eu", "us"} {
		if _, err := os.Stat(filepath.Join(states, "terraform.tfstate.d", h, "terraform.tfstate")); err != nil {
			t.Errorf("state of %s: %v", h, err)
		}
	}
	// A dry-run on existing workspaces only reads them.
	if _, err := d.(plugin.DryRunner).DryRun(ctx, request(t, v1, params, "eu", "us")); err != nil {
		t.Fatal(err)
	}

	// The same version again: nothing to change.
	again, err := d.Deploy(ctx, request(t, v1, params, "eu", "us"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Targets[0].Details["changes"] != "false" {
		t.Errorf("second deploy: %+v", again.Targets[0])
	}

	// A version that removes a VM is refused by max_destroy, before applying.
	v2 := map[string]string{"main.tf": `terraform {
  backend "local" {}
}
variable "cidr" { type = string }
variable "token" {
  type      = string
  sensitive = true
}
resource "terraform_data" "vm" {
  count = 1
  input = "${var.cidr}-${count.index}"
}
`}
	_, err = d.Deploy(ctx, request(t, v2, params, "eu", "us"))
	if !errors.Is(err, plugin.ErrPermanent) {
		t.Fatalf("expected max_destroy to refuse the plan: %v", err)
	}
	params["max_destroy"] = "1"
	if _, err := d.Deploy(ctx, request(t, v2, params, "eu", "us")); err != nil {
		t.Fatal(err)
	}
}

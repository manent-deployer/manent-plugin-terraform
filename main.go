// Command manent-plugin-terraform is the Terraform plugin of manent, served as an external process
// through the go-plugin SDK of manent. Configuration:
//
//	plugins:
//	  terraform:
//	    external:
//	      command: /opt/manent/plugins/manent-plugin-terraform
//	      sha256: "<sha256 of the binary>"
//	    artifact_types: [terraform]
package main

import (
	"github.com/manent-deployer/manent/pkg/plugin/external"

	"github.com/manent-deployer/manent-plugin-terraform/terraform"
)

func main() {
	external.Serve(terraform.Factory)
}

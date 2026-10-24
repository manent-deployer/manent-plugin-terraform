# manent-plugin-terraform

Terraform deploy plugin for [manent](https://github.com/manent-deployer/manent), shipped as an external binary ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) over gRPC) built on manent's public SDK `github.com/manent-deployer/manent/pkg/plugin/external`.

The artifact is a Terraform root module, versioned and pinned by sha256 like any other artifact. Every target of the environment is a Terraform state. A deploy runs `init`, `plan` and `apply` of that plan, with the same governance, approvals, freezes, audit, dry-run and inventory as an application release. Non-sensitive outputs become plugin outputs that other deploys can import, for example the hosts of the VMs created as targets for the next deploy.

[OpenTofu](https://opentofu.org) is the default binary; `terraform` works too.

## How a deploy runs

For every target, one after the other:

1. `init -input=false -lockfile=readonly` with `-backend-config` from the `backend.*` parameters.
2. `workspace select -or-create` when `workspace` is set.
3. `plan -detailed-exitcode -out=<file>`, then `show -json` of the plan: the step reports how many resources are added, changed and destroyed, and their addresses, never their values.
4. If the plan destroys more resources than `max_destroy` (default 0), the deploy fails without retries, before anything is applied.
5. `apply` of that exact plan file, only if the plan has changes.
6. `output -json`: non-sensitive outputs become plugin outputs.

A dry-run stops after step 4. Running the same version again plans no changes and applies nothing, so retries and recoveries are safe.

## Process isolation

- Every run has its own working directory, `HOME`, `TF_DATA_DIR` and CLI configuration file, removed at the end. Nothing is read from the worker's home or from a host CLI configuration.
- The process gets only `PATH`, `TZ` and `LANG` from the worker, plus the variables the plugin sets: the worker's own environment never reaches Terraform.
- Secrets are resolved from manent at every run and passed only in the environment of the process. They are redacted from error messages and never appear in step outputs. Sensitive outputs are never exported.
- The state never passes through manent: use a remote backend with locking (S3, `azurerm`, `gcs`, `pg`, `consul`, `http`), encrypted and readable only by the workers' identities.
- The module must contain `.terraform.lock.hcl`: providers are installed only with the versions and checksums it pins.

## Configuration

manent configuration (`manent.yml`):

```yaml
plugins:
  terraform:
    external:
      command: /usr/local/bin/manent-plugin-terraform
      sha256: "<sha256 of the binary>"
    artifact_types: [terraform]
    binary: tofu                              # default; or terraform, or an absolute path
    plugin_cache_dir: /var/cache/manent/tf    # optional provider cache shared by the runs
    provider_mirror: https://mirror.example.com/providers/   # optional, for isolated environments
```

Rollback is manual (a new apply of the previous version), so leave the `terraform` type out of `rollback.artifact_types`: types that are not listed never roll back automatically.

Environment configuration (for example `infra/network/prod.yml` in Consul or Git):

```yaml
plugin: terraform
targets:                                       # one target = one state
  - {host: prod-eu, labels: {region: eu-1}}
  - {host: prod-us, labels: {region: us-1}}
params:
  workspace: "{host}"
  backend.bucket: acme-tfstate
  backend.key: network/{environment}/{host}.tfstate
  backend.region: eu-west-1
  var.cidr: 10.20.0.0/16
  secret_var.db_password: db-password          # TF_VAR_db_password from a secret
  env.AWS_ACCESS_KEY_ID: aws-id                # name of a secret
  env.AWS_SECRET_ACCESS_KEY: aws-secret
  max_destroy: "0"
secrets:
  - {name: aws-id, ref: "vault:secret/data/infra/aws#id"}
  - {name: aws-secret, ref: "vault:secret/data/infra/aws#secret"}
  - {name: db-password, ref: "vault:secret/data/infra/db#password"}
exports:
  vms: {from: deploy.vm_hosts, format: targets}
```

The deploy descriptor names only application, environment and artifact:

```yaml
apiVersion: manent/v1
application: infra
environment: prod
artifact:
  type: terraform
  name: network
  version: 1.4.0
  source: https://artifacts.example.com/infra/network-1.4.0.tgz
```

### Parameters

| Parameter | Description |
|---|---|
| `workspace` | Workspace of each target, created if missing. Empty: the default workspace. |
| `backend.<key>` | Backend setting passed as `-backend-config=<key>=<value>`. |
| `var.<name>` | Variable passed as `TF_VAR_<name>`. |
| `secret_var.<name>` | Variable passed as `TF_VAR_<name>`, with the value of the named secret. |
| `env.<NAME>` | Environment variable with the value of the named secret (provider and backend credentials). Variables set by the plugin cannot be overridden. |
| `max_destroy` | Maximum number of resources a plan may destroy (default `0`). A replacement counts as a destroy. |
| `module_dir` | Directory of the root module inside the artifact (default: the root, or its only top-level directory). |
| `parallelism` | `-parallelism` of `plan` and `apply` (1-256). |
| `timeout` | Maximum duration of the run of one target (default `1h`). |
| `require_lock_file` | `false` allows a module without `.terraform.lock.hcl` (default `true`). |

`workspace`, `backend.*` and `var.*` accept the placeholders `{host}`, `{application}`, `{artifact}`, `{environment}` and `{tenant}`. Two targets that resolve to the same workspace and backend settings are refused: they would share one state.

Unknown parameters are refused, so a typo never goes unnoticed. Limit what descriptors may set with manent's `descriptor_policy.params`: for example allow `var.*`, but keep `backend.*` and `env.*` in the environment configuration only.

### Outputs

With one target the plugin outputs are the Terraform output names. With several targets each output is reported as `<host>.<name>`, plus `<name>` when every target has the same value. Strings are kept as they are; lists and objects become compact JSON, so a list of hosts can be exported with `format: targets`.

## The artifact

A `.tgz` with the root module and its lock file:

```sh
tofu init -backend=false          # writes .terraform.lock.hcl
tofu providers lock -platform=linux_amd64 -platform=linux_arm64
tar -czf network-1.4.0.tgz --exclude=.terraform main.tf variables.tf outputs.tf .terraform.lock.hcl
```

## Wave rollouts

Targets with `region` and `az` labels work with manent's wave rollouts: each wave, stage and batch receives only its targets, so states are changed one region at a time, with bake times, health gates and approvals in between.

## Not supported (yet)

- `terraform destroy` as a manent operation.
- Automatic rollback: a failed apply leaves the state as Terraform left it. Rollback is manual, with `manent rollback`, which applies the previously released version of the module.
- Applying exactly the plan approved in a dry-run, and drift detection in manent's reconciliation, are planned.

## Build and test

```sh
make build     # bin/manent-plugin-terraform
make test      # unit tests with a fake binary
MANENT_TEST_TOFU=$(command -v tofu) go test ./...   # also against a real binary, without downloads
```

## License

Apache-2.0, see [LICENSE](LICENSE).

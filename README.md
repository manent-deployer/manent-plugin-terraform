# manent-plugin-terraform

Terraform deploy plugin for [manent](https://github.com/manent-deployer/manent), shipped as an external binary ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) over gRPC) built on manent's public SDK `github.com/manent-deployer/manent/pkg/plugin/external`.

> **Status:** scaffolding. The plugin builds and registers, but `Deploy` refuses every request until it is implemented.

## Planned behaviour

- Artifact of type `terraform`: a `.tgz` with a root module and its `.terraform.lock.hcl`, pinned by sha256.
- Binary: OpenTofu by default, `binary` configurable to use a `terraform` provided by the user; run with an empty environment.
- One target = one state (workspace or state key); `region`/`az` labels allow wave rollouts with one state per region.
- State only in a remote backend with locking (`backend.<key>` parameters as `-backend-config`), never in manent.
- `var.<name>` as `TF_VAR_<name>`, `secret_var.<name>` and `env.<NAME>` resolved from secrets at every run.
- Dry-run: `init` + `plan -detailed-exitcode`, with only counts and resource addresses in the output.
- Deploy: `init` + `plan -out` + `apply` of that plan; `max_destroy` (default 0) refuses a plan that destroys more.
- Non-sensitive outputs (`output -json`) become plugin outputs, exportable to other deploys.
- Rollback: manual only (a new apply of the previous module version); no `destroy`.

## Build

```sh
make build     # bin/manent-plugin-terraform
make test
```

## Configuration

```yaml
plugins:
  terraform:
    external:
      command: /opt/manent/plugins/manent-plugin-terraform
      sha256: "<sha256 of the binary>"
    artifact_types: [terraform]
```

manent starts the process at the first deploy, verifies its sha256 at every start and passes secrets only through `req.Secrets`, for the duration of a call. See [External plugins](https://manent-deployer.github.io/manent/plugins/esterni/).

## License

Apache-2.0, see [LICENSE](LICENSE).

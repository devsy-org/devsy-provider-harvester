# Harvester Provider for Devsy

Run [Devsy](https://github.com/devsy-org/devsy) workspaces on Linux virtual machines provisioned by [Harvester](https://harvesterhci.io/).

## Getting started

You need a Harvester kubeconfig, an imported Harvester image, and network access from the Devsy client to the VM over SSH. The Linux guest needs SSH, Docker, and an authorized public key. Install OpenSSH on the client. The provider downloads a pinned `kubectl` for the client platform.

Add the provider and configure its connection:

```sh
devsy provider add github.com/devsy-org/devsy-provider-harvester --use=false
devsy provider set harvester -o KUBECONFIG=/path/to/kubeconfig -o HARVESTER_NAMESPACE=default
```

Setting the options initializes the provider. In Devsy Desktop, reopen its options after setting the kubeconfig and image namespace, then select an imported, ready image. A new VM requires an image selection. Configure SSH access and a cloud-init source as needed, then start a workspace:

```sh
devsy workspace up . --provider harvester
```

The Harvester account needs access to the selected images, virtual machines, storage, and any configured networks or cloud-init templates.

## Configuration

Set options in Devsy Desktop or with `devsy provider set`. See [provider.yaml](provider.yaml) for all options and defaults. Important settings include:

| Option | Purpose |
| --- | --- |
| `KUBECONFIG` | Path to the Harvester kubeconfig on the client. |
| `HARVESTER_CONTEXT` | Optional kubeconfig context. |
| `HARVESTER_NAMESPACE` | Namespace for the VM. |
| `HARVESTER_IMAGE_NAMESPACE` | Namespace containing imported images. |
| `HARVESTER_NETWORK` | Existing VM network attachment reachable from the client. |
| `HARVESTER_CPU`, `HARVESTER_MEMORY`, `HARVESTER_DISK` | Resources for new VMs. |
| `HARVESTER_SSH_USER`, `HARVESTER_SSH_PUBLIC_KEY` | Guest SSH user and public key. |
| `HARVESTER_KUBECTL_PATH` | Optional absolute path to a client-side kubectl executable. |

For a fresh Ubuntu 24.04 cloud image, `HARVESTER_CLOUD_INIT_PROFILE=ubuntu-24.04-docker-v1` enables the bundled Docker bootstrap and requires `HARVESTER_SSH_PUBLIC_KEY`. Alternatively, provide one complete cloud-init source through `HARVESTER_CLOUD_INIT_TEMPLATE`, `HARVESTER_USER_DATA_FILE`, or `HARVESTER_USER_DATA`. Custom sources must configure the guest SSH key and Docker. VM image, resource, network, and cloud-init changes apply to new VMs; existing VMs are not reconfigured.

The provider releases include helpers for Linux and macOS on amd64 and arm64, and Windows on amd64. Kubeconfig and SSH key paths refer to the client running Devsy.

## Development

```sh
go test ./...
go vet ./...
go build ./cmd/harvester-provider
```

## License and attribution

The provider is distributed under the [MIT License](LICENSE) and was originally created by [Josh Yorko](https://github.com/joshyorko) in [devsy-provider-harvester](https://github.com/joshyorko/devsy-provider-harvester). The Harvester icon is from Harvester UI and is covered by the [third-party license](THIRD_PARTY_LICENSES.md).

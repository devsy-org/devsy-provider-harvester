# Managed SSH bootstrap

Managed mode generates a client-local machine key, injects only its public key through cloud-init, and selects the private key automatically. You do not provide a private-key path or load a personal key into an SSH agent. Manual mode remains the default for existing configurations.

Choose an imported cloud image, a network reachable from your Devsy client, and a matching bootstrap/runtime pair:

| Cloud image | Bootstrap profile | Container runtime |
| --- | --- | --- |
| Ubuntu 24.04 cloud image | `ubuntu-24.04-docker-v1` | `docker` |
| Rocky Linux 9 cloud image | `rocky-9-podman-v1` | `podman`, rootful |

After selecting the image and network, set the options in Desktop or with the CLI:

```sh
devsy provider set harvester \
  -o HARVESTER_SSH_MODE=managed \
  -o HARVESTER_CONTAINER_RUNTIME=docker \
  -o HARVESTER_CLOUD_INIT_PROFILE=ubuntu-24.04-docker-v1
```

Leave `HARVESTER_SSH_KEY_PATH` empty and remove `-i`/`IdentityFile` from `HARVESTER_SSH_FLAGS`. Other flags, such as a proxy, remain available. Without a selected cloud-init source, managed mode chooses the Ubuntu/Docker profile. It does not detect/import the OS or install from an ISO.

Managed mode requires Kubernetes permission to **get the selected namespace**, in addition to existing VM/PVC permissions. Namespace metadata provides stable identity across kubeconfig/context aliases. This is cluster-scoped; constrain `namespaces/get` with `resourceNames`. Manual mode has no new namespace permission requirement.

## Custom OS and runtime

Key management is independent of the OS and engine. Complete inline, local-file or ConfigMap user-data can explicitly place this marker where the generated public key belongs:

```yaml
#cloud-config
users:
  - name: developer
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - "{{HARVESTER_SSH_PUBLIC_KEY}}"
```

This snippet inserts a key; it does not install a runtime. Set `HARVESTER_SSH_USER=developer` for this example. Your complete source must configure networking/SSH and the selected engine. Install/start the guest agent when guest IP discovery needs it. Set `HARVESTER_CONTAINER_RUNTIME` to match your installation. Advanced sources can set `HARVESTER_CONTAINER_COMMAND` to a compatible guest executable; it is a path, not a shell command line.

The provider replaces only the explicit marker. It never silently merges YAML or edits a shared ConfigMap. Expanded input and compressed inline limits are checked before disk provisioning. Custom sources retain their original SSH diagnostic detail in both SSH modes. Bundled profiles redact guest output; inspect `cloud-init status --long` and `/var/log/cloud-init-output.log` inside the guest.

Devsy's Docker-compatible driver selects the computed `HARVESTER_CONTAINER_COMMAND` through `agent.docker.path` and runtime autodetection. A dynamic `agent.docker.runtime` is not used because Devsy 1.26.0 does not interpolate that field.

The remote VM agent runs as root to preserve its inactivity/shutdown daemon. The Rocky preset therefore supplies the root-owned `/usr/local/bin/devsy-podman-rootful` wrapper. Both the agent and readiness checks use it, so a non-root SSH login and the root agent address the same rootful engine. The wrapper uses noninteractive sudo when needed, resets root's container/XDG environment, and executes the local Podman binary without wrapper banners. It does not replace the system `podman` command. Use this wrapper when inspecting Devsy's containers over SSH; plain `podman` as the SSH user addresses a separate rootless storage area.

This preset does not promise rootless Podman. Merely changing the CLI user or pointing the root agent at a user socket does not fix ownership of the agent's staged files/volumes. Rootless support is separate work. Custom sources must account for this remote-agent identity model rather than assuming the SSH user is the engine user.

## Credentials and recovery

Keys live below `.ssh/devsy-harvester` in the client's user home, protected by owner-only Unix permissions or Windows ACLs. `HARVESTER_MANAGED_SSH_DIR` is an optional credential-location override, not a required key path. Exported provider/machine/workspace subdirectories and their symlink aliases are refused.

Back up this directory as private credential material. Workspace exports do not include it. Another client does not automatically acquire the key; restore it securely or deliberately authorize another key in the guest. Missing/corrupt state never causes key regeneration against an existing VM or surviving disk.

VM/PVC records bind the fingerprint, SSH user, runtime executable and bootstrap settings. Reconnect verifies both the VM and attached rootdisk without reopening creation-only files or ConfigMaps. Preserve the saved runtime command when changing creation settings: helper-side profile restoration cannot rewrite an engine path already resolved by Devsy. A disagreement is refused. PVC-only recovery requires the original key and bootstrap input; incompatible or ambiguous partial state is refused.

Failed creation and uncertain deletion retain credentials. Successful managed deletion removes the key only after the owned VM/disks are absent. Empty lock files remain for concurrent waiters. Optional `HARVESTER_EXPECTED_VM_UID` and `HARVESTER_EXPECTED_ROOTDISK_UID` fence automated cleanup to previously recorded resources; a replacement is refused before mutation and at deletion's UID precondition.

Devsy can still discard workspace/machine registrations after a provider create failure. Key retention prevents credential loss but does not fix that separate Devsy behavior. Recover using the original VM name/settings; a new workspace does not automatically discover an orphan.

## Live verification

The Linux-only `scripts/verify-managed-harvester.py` requires Devsy, kubectl, yq, OpenSSH and a helper built from this source. Run `--help` for arguments. Without `--execute`, it prints a plan and changes nothing.

Run live invocations serially with task-specific `--artifacts` directories outside the checkout. Each creates at most one VM with 2 vCPUs, 2 GiB RAM and a 12 GiB disk. It uses an isolated Devsy home/SSH configuration, disables ambient credentials, and places generated keys in a path containing a space.

Receipts distinguish real Devsy post-create and workspace read/write, selected-engine container ownership, saved-workspace reconnect, guest/engine privilege identities, disk headroom, stable identities on retry, missing-key refusal and UID-fenced cleanup. Podman must report rootless=false for this preset; its version/storage roots are recorded. The daemon is checked active with its saved 30-minute inactivity configuration, but timed shutdown is not claimed unless separately observed. Keep private test credentials out of commits and shared receipts. Linux live results, native Windows tests and hosted CI are separate gates; cross-building is not Windows acceptance.

See the [recorded Harvester acceptance receipt](verification/managed-harvester-2026-11-09.json) for the tested helper, fixture and cleanup results.

#!/usr/bin/env python3
"""Exercise one disposable Harvester VM through an isolated Devsy installation."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import subprocess
import time
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--context", required=True)
    parser.add_argument("--namespace", default="default")
    parser.add_argument("--network", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--runtime", choices=["docker", "podman"], required=True)
    parser.add_argument("--container-image", required=True)
    parser.add_argument("--helper", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if "@sha256:" not in args.container_image:
        parser.error("--container-image must be pinned by digest")
    if not args.execute:
        print("Plan: one VM, 2 vCPU, 2 GiB RAM, 12 GiB disk; verify and delete it. Pass --execute to run.")
        return
    root = args.artifacts.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    home = root / "devsy-home"
    keys = root / "managed keys"
    name = "devsy-managed-proof-" + uuid.uuid4().hex[:10]
    workspace = "managed-" + args.runtime
    env = os.environ.copy()
    env.pop("SSH_AUTH_SOCK", None)
    env.update({"HARVESTER_VM_NAME": name, "DOCKER_CONFIG": str(root / "docker-config"), "GIT_CONFIG_GLOBAL": "/dev/null"})
    (root / "docker-config").mkdir(mode=0o700)
    ssh_config = root / "ssh_config"
    ssh_config.write_text("Host *\n    IdentityAgent none\n    IdentitiesOnly yes\n")
    kubectl = ["kubectl", "--kubeconfig", args.kubeconfig, "--context", args.context, "--request-timeout=10s", "-n", args.namespace]
    devsy = ["devsy", "--home", str(home)]
    receipt = {"vm_name": name, "runtime": args.runtime, "profile": args.profile, "container_image": args.container_image,
               "budget": {"cpu": 2, "memory": "2Gi", "disk": "12Gi"}, "helper_sha256": hashlib.sha256(args.helper.read_bytes()).hexdigest(), "resources": {}}

    def save():
        (root / "receipt.json").write_text(json.dumps(receipt, indent=2) + "\n")

    def resource(kind, resource_name):
        result = subprocess.run(kubectl + ["get", kind, resource_name, "--ignore-not-found", "-o", "json"], env=env, capture_output=True, text=True, timeout=15)
        if result.returncode:
            raise RuntimeError("cannot inspect task resource: " + result.stderr.strip())
        return json.loads(result.stdout) if result.stdout.strip() else None

    def capture():
        for kind, resource_name in [("vm", name), ("pvc", name + "-rootdisk")]:
            if resource_name in receipt["resources"]:
                continue
            obj = resource(kind, resource_name)
            if obj is None:
                continue
            meta = obj["metadata"]
            if meta.get("labels", {}).get("devsy.sh/machine-id") != name or meta.get("annotations", {}).get("devsy.sh/ssh-mode") != "managed":
                raise RuntimeError("task resource does not have the expected managed ownership")
            receipt["resources"][resource_name] = {"kind": kind, "uid": meta["uid"], "annotations": meta.get("annotations", {})}
            if kind == "vm":
                domain = obj["spec"]["template"]["spec"]["domain"]
                if domain["cpu"]["cores"] != 2 or domain["memory"]["guest"] != "2Gi":
                    raise RuntimeError("test VM exceeded the agreed CPU/RAM budget")
            save()

    def run(label, command, timeout=180, observe=False, allow_failure=False):
        path = root / (label + ".log")
        print(label, flush=True)
        with path.open("w") as log:
            child = subprocess.Popen(command, env=env, stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            deadline = time.monotonic() + timeout
            try:
                while child.poll() is None:
                    if observe:
                        capture()
                    if time.monotonic() > deadline:
                        raise TimeoutError(label + " exceeded its deadline")
                    time.sleep(2)
            except BaseException:
                os.killpg(child.pid, signal.SIGTERM)
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait(timeout=5)
                raise
        output = path.read_text()
        if child.returncode and not allow_failure:
            raise RuntimeError(label + " failed; inspect " + str(path))
        return child.returncode, output

    for kind, resource_name in [("vm", name), ("pvc", name + "-rootdisk"), ("pvc", name + "-image"), ("pvc", name + "-blankdisk")]:
        if resource(kind, resource_name) is not None:
            raise RuntimeError("test name already exists; refusing to adopt it")
    save()
    manifest = json.loads(subprocess.check_output(["yq", "-o=json", ".", str(Path(__file__).resolve().parents[1] / "provider.yaml")], text=True))
    manifest["name"] = "harvester-managed-proof"
    arch = {"x86_64": "amd64", "aarch64": "arm64"}[platform.machine()]
    kubectl_path = subprocess.check_output(["which", "kubectl"], text=True).strip()
    manifest["binaries"] = {"HARVESTER_PROVIDER": [{"os": "linux", "arch": arch, "path": str(args.helper.resolve())}],
                            "KUBECTL": [{"os": "linux", "arch": arch, "path": kubectl_path}]}
    provider_file = root / "provider.yaml"
    provider_file.write_text(json.dumps(manifest, indent=2))
    ssh_flags = "-F " + str(ssh_config) + " -o UserKnownHostsFile=" + str(root / "known_hosts") + " -o GlobalKnownHostsFile=/dev/null -o IdentityAgent=none"
    options = {"KUBECONFIG": args.kubeconfig, "HARVESTER_CONTEXT": args.context, "HARVESTER_NAMESPACE": args.namespace,
               "HARVESTER_NETWORK": args.network, "HARVESTER_IMAGE_NAMESPACE": args.image.split("/")[0],
               "HARVESTER_IMAGE_CHOICE": args.image, "HARVESTER_CPU": "2", "HARVESTER_MEMORY": "2Gi", "HARVESTER_DISK": "12Gi",
               "HARVESTER_SSH_MODE": "managed", "HARVESTER_SSH_USER": "developer", "HARVESTER_MANAGED_SSH_DIR": str(keys),
               "HARVESTER_CONTAINER_RUNTIME": args.runtime, "HARVESTER_CLOUD_INIT_PROFILE": args.profile, "HARVESTER_SSH_FLAGS": ssh_flags}
    helper_env = {**env, **options, "HARVESTER_KUBECTL_PATH": kubectl_path, "PROVIDER_FOLDER": str(home / "provider")}

    def helper_command(label, operation, command=None, allow_failure=False):
        original = env.copy()
        env.update(helper_env)
        try:
            cmd = [str(args.helper.resolve()), operation]
            if command is not None:
                cmd += ["--command", command]
            return run(label, cmd, timeout=200, allow_failure=allow_failure)
        finally:
            env.clear()
            env.update(original)

    try:
        add = devsy + ["provider", "add", str(provider_file), "--name", "harvester-managed-proof"]
        for key, value in options.items():
            add += ["-o", key + "=" + value]
        run("provider-add", add)
        context_options = ["SSH_ADD_PRIVATE_KEYS=false", "SSH_AGENT_FORWARDING=false", "SSH_INJECT_DOCKER_CREDENTIALS=false",
                           "SSH_INJECT_GIT_CREDENTIALS=false", "GIT_SSH_SIGNATURE_FORWARDING=false", "TELEMETRY=false"]
        context_set = devsy + ["context", "set"]
        for option in context_options:
            context_set += ["-o", option]
        run("context-isolation", context_set)
        fixture = root / "fixture"
        (fixture / ".devcontainer").mkdir(parents=True)
        (fixture / "proof.txt").write_text("managed-source-copy\n")
        (fixture / ".devcontainer" / "devcontainer.json").write_text(json.dumps({"image": args.container_image, "postCreateCommand": "printf managed-post-create > /tmp/managed-post-create"}))
        run("workspace-up", devsy + ["workspace", "up", str(fixture), "--provider", "harvester-managed-proof", "--id", workspace, "--ide", "none", "--ide-launch", "skip", "--ssh-configure=false"], timeout=1200, observe=True)
        capture()
        if len(receipt["resources"]) != 2:
            raise RuntimeError("missing VM/PVC identity receipts")
        receipt["devsy_version"] = subprocess.check_output(["devsy", "--version"], text=True).strip()
        _, proof = run("container-exec", devsy + ["workspace", "ssh", workspace, "--agent-forwarding=false", "--start-services=false", "--command", "test \"$(cat /tmp/managed-post-create)\" = managed-post-create && test \"$(cat proof.txt)\" = managed-source-copy && printf 'CONTAINER_PROOF_OK\\n'"], timeout=180)
        if "CONTAINER_PROOF_OK" not in proof:
            raise RuntimeError("actual devcontainer execution proof missing")
        receipt["container_exec"] = "passed"
        _, guest = helper_command("guest-runtime", "command", "printf 'GUEST_UID=%s\\n' \"$(id -u)\"; cloud-init status; " + args.runtime + " ps --format '{{.ID}} {{.Image}}'; df -Pm /")
        if "GUEST_UID=0\n" in guest:
            raise RuntimeError("bootstrap verification unexpectedly ran as root")
        receipt["guest_summary"] = guest.strip()
        if args.runtime == "podman":
            _, rootless = helper_command("podman-rootless", "command", "podman info --format '{{.Host.Security.Rootless}}'")
            if rootless.strip() != "true":
                raise RuntimeError("Podman devcontainer was not proved rootless")
            receipt["rootless"] = True
        key_files = list(keys.glob("*/id_rsa"))
        if len(key_files) != 1:
            raise RuntimeError("expected exactly one generated identity")
        key_hash = hashlib.sha256(key_files[0].read_bytes()).hexdigest()
        helper_command("same-machine-retry", "create")
        if hashlib.sha256(key_files[0].read_bytes()).hexdigest() != key_hash:
            raise RuntimeError("retry changed its identity")
        for resource_name, expected in receipt["resources"].items():
            if resource(expected["kind"], resource_name)["metadata"]["uid"] != expected["uid"]:
                raise RuntimeError("retry replaced a VM or disk")
        receipt["same_identity_retry"] = "passed"
        saved_key = key_files[0].with_name("identity-backup")
        key_files[0].rename(saved_key)
        try:
            code, _ = helper_command("missing-key-refused", "create", allow_failure=True)
            if code == 0 or key_files[0].exists():
                raise RuntimeError("missing-key reconnect was not fail-closed")
            receipt["missing_key_refusal"] = "passed"
        finally:
            saved_key.rename(key_files[0])
        save()
    finally:
        capture()
        if receipt["resources"]:
            helper_command("provider-delete", "delete")
            for resource_name, expected in receipt["resources"].items():
                if resource(expected["kind"], resource_name) is not None:
                    raise RuntimeError("task resource survived provider deletion")
            if list(keys.glob("*/id_rsa")):
                raise RuntimeError("private key survived confirmed resource deletion")
            receipt["cleanup"] = "VM, PVC and managed private key removed"
        else:
            receipt["cleanup"] = "no task resources created"
        save()
    receipt["result"] = "passed"
    save()
    print(json.dumps(receipt, indent=2), flush=True)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Exercise one disposable Harvester VM through an isolated Devsy installation."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shlex
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
    if platform.system() != "Linux":
        parser.error("live acceptance requires a Linux client; qualify native Windows separately")
    root = args.artifacts.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    home = root / "devsy-home"
    keys = root / "managed keys"
    name = "devsy-managed-proof-" + uuid.uuid4().hex[:10]
    workspace = "managed-" + args.runtime
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("HARVESTER_", "DEVSY_")) and key not in ("SSH_AUTH_SOCK", "KUBECONFIG", "KUBECTL")}
    env.update({"HARVESTER_VM_NAME": name, "DOCKER_CONFIG": str(root / "docker-config"), "GIT_CONFIG_GLOBAL": "/dev/null"})
    (root / "docker-config").mkdir(mode=0o700)
    ssh_config = root / "ssh_config"
    ssh_config.write_text("Host *\n    IdentityAgent none\n    IdentitiesOnly yes\n")
    kubectl = ["kubectl", "--kubeconfig", args.kubeconfig, "--context", args.context, "--request-timeout=10s", "-n", args.namespace]
    devsy = ["devsy", "--home", str(home), "--log-level", "info", "--log-output", "text"]
    receipt = {"vm_name": name, "runtime": args.runtime, "profile": args.profile, "container_image": args.container_image,
               "budget": {"cpu": 2, "memory": "2Gi", "disk": "12Gi"}, "helper_sha256": hashlib.sha256(args.helper.read_bytes()).hexdigest(), "resources": {}}
    selected_command = None

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
            if meta.get("labels", {}).get("devsy.sh/provider") != "harvester" or meta.get("labels", {}).get("devsy.sh/machine-id") != name or meta.get("annotations", {}).get("devsy.sh/ssh-mode") != "managed":
                raise RuntimeError("task resource does not have the expected managed ownership")
            if selected_command is not None and meta.get("annotations", {}).get("devsy.sh/container-command") != selected_command:
                raise RuntimeError("provider and verifier resolved different runtime commands")
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
    receipt["inactivity_timeout"] = manifest["agent"].get("inactivityTimeout")
    receipt["shutdown_configured"] = bool(manifest["agent"].get("exec", {}).get("shutdown"))
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

    def helper_command(label, operation, command=None, allow_failure=False, timeout=200):
        original = env.copy()
        env.update(helper_env)
        try:
            cmd = [str(args.helper.resolve()), operation]
            if command is not None:
                cmd += ["--command", command]
            return run(label, cmd, timeout=timeout, allow_failure=allow_failure)
        finally:
            env.clear()
            env.update(original)

    try:
        _, command_output = helper_command("runtime-command", "runtime-command", timeout=10)
        selected_command = command_output.strip()
        if not selected_command or "\n" in selected_command:
            raise RuntimeError("runtime command selector did not return one executable")
        engine = shlex.quote(selected_command)
        receipt["runtime_command"] = selected_command
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
        (fixture / "proof.txt").write_text(name + "\n")
        (fixture / ".devcontainer" / "devcontainer.json").write_text(json.dumps({"image": args.container_image, "remoteUser": "root", "postCreateCommand": "printf '%s' " + shlex.quote(name) + " > /tmp/managed-post-create"}))
        run("workspace-up", devsy + ["workspace", "up", str(fixture), "--provider", "harvester-managed-proof", "--id", workspace, "--ide", "none", "--ide-launch", "skip", "--ssh-configure=false"], timeout=1200, observe=True)
        capture()
        if len(receipt["resources"]) != 2:
            raise RuntimeError("missing VM/PVC identity receipts")
        receipt["devsy_version"] = subprocess.check_output(["devsy", "--version"], text=True).strip()
        marker_check = 'test "$(cat /tmp/managed-post-create)" = ' + shlex.quote(name)
        _, proof = run("container-exec", devsy + ["workspace", "ssh", workspace, "--agent-forwarding=false", "--start-services=false", "--command", marker_check + ' && test "$(cat proof.txt)" = ' + shlex.quote(name) + " && printf '%s' " + shlex.quote(name) + ' > acceptance-write.txt && test "$(cat acceptance-write.txt)" = ' + shlex.quote(name) + " && printf 'CONTAINER_PROOF_OK\\n'"], timeout=180)
        if "CONTAINER_PROOF_OK" not in proof:
            raise RuntimeError("actual devcontainer execution proof missing")
        receipt["container_exec"] = "passed"
        receipt["workspace_read_write"] = "passed"
        ownership_command = 'for candidate in $(' + engine + ' ps -q); do ' + engine + ' exec --user root "$candidate" sh -c ' + shlex.quote(marker_check) + ' >/dev/null 2>&1 && { printf "ENGINE_CONTAINER_ID=%s\\n" "$candidate"; exit 0; }; done; exit 1'
        _, ownership = helper_command("engine-container-ownership", "command", ownership_command)
        container_id = re.search(r"(?m)^ENGINE_CONTAINER_ID=([a-f0-9]{12,64})$", ownership)
        if container_id is None:
            raise RuntimeError("selected runtime did not prove ownership of Devsy's actual container")
        receipt["engine_container_id"] = container_id[1]
        _, guest = helper_command("guest-runtime", "command", "printf 'GUEST_UID=%s\\n' \"$(id -u)\"; sudo -n cloud-init status; " + engine + " ps --format '{{.ID}} {{.Image}}'; printf 'FREE_KIB='; df -Pk / | awk 'NR==2 {print $4}'")
        guest_uid = re.search(r"(?m)^GUEST_UID=([0-9]+)$", guest)
        free_space = re.search(r"(?m)^FREE_KIB=([0-9]+)$", guest)
        if guest_uid is None or int(guest_uid[1]) == 0:
            raise RuntimeError("bootstrap verification did not prove a non-root guest user")
        if free_space is None or int(free_space[1]) < 512 * 1024:
            raise RuntimeError("guest disk has less than 512 MiB available after devcontainer startup")
        receipt["guest_uid"], receipt["free_kib"] = int(guest_uid[1]), int(free_space[1])
        receipt["guest_summary"] = guest.strip()
        _, daemon = helper_command("vm-daemon", "command", "sudo -n systemctl is-active --quiet devsy.service && printf 'DAEMON_ACTIVE\\n'")
        if "DAEMON_ACTIVE" not in daemon or receipt["inactivity_timeout"] != "30m" or not receipt["shutdown_configured"]:
            raise RuntimeError("VM daemon or saved inactivity configuration was not preserved")
        receipt["vm_daemon_active"] = True
        if args.runtime == "podman":
            _, info_output = helper_command("podman-engine-mode", "command", engine + " info --format json")
            try:
                info, _ = json.JSONDecoder().raw_decode(info_output[info_output.index("{"):])
                rootless = info["host"]["security"]["rootless"]
                store = info["store"]
                version = info["version"].get("Version") or info["version"].get("version")
                if rootless is not False or not version or not store["graphRoot"] or not store["runRoot"]:
                    raise ValueError("unexpected Podman engine identity")
            except (KeyError, ValueError, TypeError) as error:
                raise RuntimeError("preset did not prove its rootful Podman engine identity") from error
            receipt.update({"rootless": False, "engine_version": version, "graph_root": store["graphRoot"], "run_root": store["runRoot"]})
        key_files = list(keys.glob("*/id_rsa"))
        if len(key_files) != 1:
            raise RuntimeError("expected exactly one generated identity")
        fingerprint = subprocess.check_output(["ssh-keygen", "-lf", str(key_files[0]), "-E", "sha256"], text=True, env=env, timeout=10).split()[1]
        if any(value["annotations"].get("devsy.sh/ssh-fingerprint") != fingerprint for value in receipt["resources"].values()):
            raise RuntimeError("cluster annotations do not match the generated client key")
        receipt["key_fingerprint"] = fingerprint
        key_hash = hashlib.sha256(key_files[0].read_bytes()).hexdigest()
        helper_command("same-machine-retry", "create")
        if hashlib.sha256(key_files[0].read_bytes()).hexdigest() != key_hash:
            raise RuntimeError("retry changed its identity")
        for resource_name, expected in receipt["resources"].items():
            if resource(expected["kind"], resource_name)["metadata"]["uid"] != expected["uid"]:
                raise RuntimeError("retry replaced a VM or disk")
        receipt["same_identity_retry"] = "passed"
        run("workspace-reopen", devsy + ["workspace", "up", workspace, "--id", workspace, "--provider", "harvester-managed-proof", "--ide", "none", "--ide-launch", "skip", "--ssh-configure=false"], timeout=240)
        _, reopened = helper_command("engine-container-reopen", "command", ownership_command)
        reopened_id = re.search(r"(?m)^ENGINE_CONTAINER_ID=([a-f0-9]{12,64})$", reopened)
        if reopened_id is None or reopened_id[1] != receipt["engine_container_id"]:
            raise RuntimeError("saved workspace reconnect changed its engine/container identity")
        receipt["saved_workspace_reopen"] = "passed"
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
    except Exception as error:
        receipt["result"], receipt["failure"] = "failed", str(error)
        save()
        try:
            helper_command("failure-diagnostics", "command", "sudo -n cloud-init status --long; sudo -n tail -80 /var/log/cloud-init-output.log; id; " + args.runtime + " info", allow_failure=True, timeout=30)
        except Exception:
            receipt["failure_diagnostics"] = "guest diagnostics unavailable; inspect the launch log"
            save()
        raise
    finally:
        capture()
        if receipt["resources"]:
            for resource_name, expected in receipt["resources"].items():
                current = resource(expected["kind"], resource_name)
                if current is not None and current["metadata"]["uid"] != expected["uid"]:
                    receipt["result"], receipt["cleanup"] = "failed", "refused: a recorded resource was replaced"
                    save()
                    raise RuntimeError("task resource UID changed; refusing to delete the replacement")
            helper_env["HARVESTER_EXPECTED_VM_UID"] = receipt["resources"].get(name, {}).get("uid", "")
            helper_env["HARVESTER_EXPECTED_ROOTDISK_UID"] = receipt["resources"].get(name + "-rootdisk", {}).get("uid", "")
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
    if hashlib.sha256(args.helper.read_bytes()).hexdigest() != receipt["helper_sha256"]:
        raise RuntimeError("provider binary changed during live acceptance; repeat against one immutable build")
    receipt["result"] = "passed"
    save()
    print(json.dumps(receipt, indent=2), flush=True)


if __name__ == "__main__":
    main()

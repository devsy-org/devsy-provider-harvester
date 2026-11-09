import importlib.util
import contextlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


class AcceptanceTests(unittest.TestCase):
    def exercise(self, replacement=False, empty_engine=False, assert_hermetic=False):
        spec = importlib.util.spec_from_file_location("verification", Path(__file__).with_name("verify-managed-harvester.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with tempfile.TemporaryDirectory(prefix="codex-harvester-acceptance-test-") as directory:
            root = Path(directory)
            helper = root / "helper"
            helper.write_text("test binary; never executed")
            artifacts = root / "artifacts"
            state, deleted, leaked = {}, [], []

            class Child:
                def __init__(self, command, *, env, stdin, stdout, stderr, start_new_session):
                    self.returncode = 0
                    label = Path(stdout.name).stem
                    if assert_hermetic:
                        leaked.extend(key for key in ["HARVESTER_SSH_HOST", "DEVSY_AGENT_URL"] if key in env)
                    name = env["HARVESTER_VM_NAME"]
                    key = artifacts / "managed keys" / "slot" / "id_rsa"
                    if label == "workspace-up":
                        metadata = {"name": name, "uid": "original-vm", "labels": {"devsy.sh/provider": "harvester", "devsy.sh/machine-id": name},
                                    "annotations": {"devsy.sh/ssh-mode": "managed", "devsy.sh/ssh-fingerprint": "SHA256:matching"}}
                        state[name] = {"metadata": metadata, "spec": {"template": {"spec": {"domain": {"cpu": {"cores": 2}, "memory": {"guest": "2Gi"}}}}}}
                        state[name + "-rootdisk"] = {"metadata": {**metadata, "name": name + "-rootdisk", "uid": "original-pvc"}}
                        key.parent.mkdir(parents=True)
                        key.write_text("synthetic identity; not a private key")
                    elif label == "container-exec":
                        stdout.write("CONTAINER_PROOF_OK\n")
                    elif label == "guest-runtime":
                        stdout.write("GUEST_UID=1000\nstatus: done\nFREE_KIB=8388608\n")
                    elif label == "engine-container-ownership":
                        if empty_engine:
                            self.returncode = 1
                        else:
                            stdout.write("ENGINE_CONTAINER_ID=" + "a" * 12 + "\n")
                    elif label == "podman-rootless":
                        stdout.write("true\n")
                    elif label == "missing-key-refused":
                        self.returncode = 1
                        if replacement:
                            state[name]["metadata"]["uid"] = "replacement-vm"
                    elif label == "provider-delete":
                        deleted.extend(obj["metadata"]["uid"] for obj in state.values())
                        state.clear()
                        key.unlink()

                def poll(self):
                    return self.returncode

            def run(command, **kwargs):
                index = command.index("get")
                obj = state.get(command[index + 2])
                return subprocess.CompletedProcess(command, 0, json.dumps(obj) if obj else "", "")

            def output(command, **kwargs):
                if command[0] == "yq":
                    return "{}"
                if command[0] == "which":
                    return "/not-executed/kubectl\n"
                if command[0] == "ssh-keygen":
                    return "2048 SHA256:matching synthetic (RSA)\n"
                if command[0] == "devsy":
                    return "v-test\n"
                raise AssertionError("unmocked external command")

            argv = ["verify", "--execute", "--kubeconfig", "/not-used", "--context", "mock", "--network", "default/mock", "--image", "default/mock",
                    "--profile", "rocky-9-podman-v1", "--runtime", "podman", "--container-image", "example@sha256:" + "0" * 64,
                    "--helper", str(helper), "--artifacts", str(artifacts)]
            failure = None
            with patch.object(sys, "argv", argv), patch.object(module.subprocess, "run", run), patch.object(module.subprocess, "check_output", output), patch.object(module.subprocess, "Popen", Child), patch.object(module.platform, "machine", return_value="x86_64"), patch.object(module.platform, "system", return_value="Linux"):
                try:
                    with contextlib.redirect_stdout(io.StringIO()):
                        module.main()
                except RuntimeError as error:
                    failure = str(error)
            receipt = json.loads((artifacts / "receipt.json").read_text())
            self.assertEqual(leaked, [], "ambient provider/agent settings escaped isolation")
            return receipt, deleted, failure

    def test_replaced_vm_is_not_passed_to_cleanup(self):
        receipt, deleted, failure = self.exercise(replacement=True)
        self.assertIsNotNone(failure)
        self.assertNotIn("replacement-vm", deleted)
        self.assertNotEqual(receipt.get("result"), "passed")

    def test_empty_selected_engine_does_not_prove_podman_ownership(self):
        receipt, _, failure = self.exercise(empty_engine=True)
        self.assertIsNotNone(failure)
        self.assertNotEqual(receipt.get("result"), "passed")

    def test_matching_resources_and_container_complete(self):
        receipt, deleted, failure = self.exercise()
        self.assertIsNone(failure)
        self.assertEqual(receipt.get("result"), "passed")
        self.assertEqual(set(deleted), {"original-vm", "original-pvc"})

    def test_ambient_provider_settings_are_not_forwarded(self):
        with patch.dict("os.environ", {"HARVESTER_SSH_HOST": "192.0.2.25", "DEVSY_AGENT_URL": "not-used://wrong-agent"}):
            receipt, _, failure = self.exercise(assert_hermetic=True)
        self.assertIsNone(failure)
        self.assertEqual(receipt.get("result"), "passed")


if __name__ == "__main__":
    unittest.main()

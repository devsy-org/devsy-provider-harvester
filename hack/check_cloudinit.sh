#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
fixture_dir=$(mktemp -d)
trap 'rm -rf "$fixture_dir"' EXIT
HARVESTER_TEST_SCHEMA_OUTPUT="$fixture_dir/user-data.yaml" go test ./cmd/harvester-provider -run '^TestBundledSchemaFixture$' -count=1
docker run --rm -v "$fixture_dir:/work:ro,Z" ubuntu:24.04 bash -euc '
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq cloud-init >/tmp/install.log 2>&1 || { cat /tmp/install.log; exit 1; }
  cloud-init --version
  cloud-init schema -c /work/user-data.yaml --annotate
  python3 -c '\''import yaml, subprocess; data = yaml.safe_load(open("/work/user-data.yaml")); subprocess.run(["bash", "-n"], input=data["write_files"][0]["content"], text=True, check=True)'\''
'

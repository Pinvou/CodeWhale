"""Disposable Linux CI integration test; never use for a production deployment."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

HERE = Path(__file__).resolve().parent
PLATFORM = HERE.parent.parent


def run(args, *, timeout=120, check=True, env=None):
    result = subprocess.run(args, check=False, timeout=timeout, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, env=env)
    if check and result.returncode:
        print(result.stdout[-12000:], file=sys.stderr)
        raise subprocess.CalledProcessError(result.returncode, args)
    return result


def docker(*args, **kwargs):
    return run(["docker", *args], **kwargs)


def prepare_wheels(root):
    bindings = json.loads((HERE / "pdf-wheel-bindings.json").read_text())
    expected = [entry for entry in bindings if entry["platform"] == "x86_64"]
    if {entry["name"] for entry in expected} != {"lxml", "packaging", "pikepdf", "pillow"} or len(expected) != 4:
        raise ValueError("unexpected amd64 wheel bindings")
    wheels = root / "x86_64"
    wheels.mkdir(parents=True)
    run([sys.executable, "-I", "-B", "-m", "pip", "--isolated", "download",
         "--index-url", "https://pypi.org/simple", "--only-binary=:all:",
         "--require-hashes", "--no-deps", "--platform", "manylinux_2_28_x86_64",
         "--python-version", "3.12", "--implementation", "cp", "--abi", "cp312",
         "--dest", str(wheels), "-r", str(HERE / "pdf-requirements.lock")], timeout=240)
    if {p.name for p in wheels.iterdir()} != {item["filename"] for item in expected}:
        raise ValueError("downloaded wheel set differs from the fixed binding")
    for item in expected:
        if hashlib.sha256((wheels / item["filename"]).read_bytes()).hexdigest() != item["sha256"]:
            raise ValueError("wheel digest mismatch")
    (root / "manifest.json").write_text(json.dumps({"wheels": expected}), encoding="utf-8")
    run([sys.executable, "-I", "-B", str(HERE / "prepare_pdf_image.py"),
         "--audit-dir", str(root), "--platform", "linux/amd64"])
    return wheels


def main():
    if sys.platform != "linux" or os.uname().machine != "x86_64":
        raise RuntimeError("this job requires native Linux x86_64")
    if not Path("/sys/fs/cgroup/cgroup.controllers").is_file():
        raise RuntimeError("this job requires cgroup v2")
    docker("info", timeout=30)
    token = uuid.uuid4().hex
    owner = "pinvou.ci.owner=" + token
    prefix = "pinvou-evidence-ci-" + token
    image = prefix + ":local"
    containers = [prefix + "-init", prefix + "-worker", prefix + "-client", prefix + "-bad", prefix + "-restart", prefix + "-check", prefix + "-client-after"]
    volume = prefix + "-socket"
    try:
        with tempfile.TemporaryDirectory(prefix="pinvou-evidence-ci-") as temp:
            root = Path(temp)
            wheels = prepare_wheels(root / "audit")
            if docker("image", "inspect", image, check=False).returncode == 0:
                raise RuntimeError("generated CI image tag already exists")
            # An explicit named build context prevents BuildKit resolving a registry image.
            docker("buildx", "build", "--platform", "linux/amd64", "--load", "--tag", image, "--label", owner,
                   "--build-context", "pdf-wheels=" + str(wheels),
                   "--file", str(PLATFORM / "Dockerfile.evidence-validator-pdf"), str(PLATFORM), timeout=1200)
            print("PDF image built and loaded:", docker("image", "inspect", "--format", "{{.Id}}", image).stdout.strip())
            env = os.environ.copy()
            env.update(GOOS="linux", GOARCH="amd64", CGO_ENABLED="0", GOTOOLCHAIN="local")
            binary = root / "billing-container.test"
            build_test_binary(binary, env)
            # A private CI-only named volume holds only a socket, never customer material.
            if docker("volume", "inspect", volume, check=False).returncode == 0:
                raise RuntimeError("generated CI volume already exists")
            docker("volume", "create", "--label", owner, volume)
            label = docker("volume", "inspect", "--format", '{{ index .Labels "pinvou.ci.owner" }}', volume).stdout.strip()
            if label != token:
                raise RuntimeError("CI volume ownership label does not match")
            mount = "type=volume,src=" + volume + ",dst=/run/evidence"
            common = ["--network", "none", "--read-only", "--cap-drop", "ALL",
                      "--security-opt", "no-new-privileges", "--label", owner]
            initialize_volume(containers[0], common, mount, image)
            docker("run", "--detach", "--name", containers[1], *common,
                   "--memory", "2g", "--memory-swap", "2g", "--cpus", "1", "--pids-limit", "32",
                   "--user", "65533:65533", "--mount", mount,
                   "--env", "EVIDENCE_IMAGE_VALIDATOR_ENABLED=true",
                   "--env", "EVIDENCE_PDF_CANDIDATE_ENABLED=true",
                   "--env", "EVIDENCE_IMAGE_VALIDATOR_SOCKET=/run/evidence/validator.sock",
                   "--env", "EVIDENCE_IMAGE_VALIDATOR_ALLOWED_UID=65532", image)
            worker_ready(containers[1])
            inspect = json.loads(docker("inspect", containers[1]).stdout)[0]
            config = inspect["HostConfig"]
            if (config["NetworkMode"] != "none" or not config["ReadonlyRootfs"] or
                    config["Memory"] != 2 * 1024**3 or config["MemorySwap"] != 2 * 1024**3 or
                    config["PidsLimit"] != 32 or "ALL" not in config["CapDrop"] or
                    not any(value in ("no-new-privileges", "no-new-privileges:true") for value in config["SecurityOpt"])):
                raise RuntimeError("container settings do not match requested isolation")
            status = docker("exec", containers[1], "/bin/cat", "/proc/1/status").stdout
            fields = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
            if (fields.get("NoNewPrivs", "").strip() != "1" or
                    int(fields.get("CapEff", "-1").strip(), 16) != 0 or
                    fields.get("Uid", "").split()[:2] != ["65533", "65533"]):
                raise RuntimeError("worker process identity or privilege flags differ")
            cgroup = docker("exec", containers[1], "/bin/cat", "/sys/fs/cgroup/memory.max").stdout.strip()
            if cgroup != str(2 * 1024**3):
                raise RuntimeError("actual cgroup memory.max differs from requested value")
            if docker("exec", containers[1], "/bin/cat", "/sys/fs/cgroup/memory.swap.max").stdout.strip() != "0":
                raise RuntimeError("actual cgroup swap limit differs from requested value")
            if docker("exec", containers[1], "/bin/cat", "/sys/fs/cgroup/pids.max").stdout.strip() != "32":
                raise RuntimeError("actual cgroup pids.max differs from requested value")
            cpu = docker("exec", containers[1], "/bin/cat", "/sys/fs/cgroup/cpu.max").stdout.strip().split()
            if len(cpu) != 2 or not all(value.isdecimal() for value in cpu) or int(cpu[0]) != int(cpu[1]):
                raise RuntimeError("actual cgroup cpu.max does not match one CPU")
            docker("exec", containers[1], "/opt/pinvou/pdf/bin/python3", "-I", "-B", "-c",
                   "import socket; assert {name for _,name in socket.if_nameindex()} == {'lo'}")
            check_peer_access(containers, owner, image, volume, binary)
            # The existing socket remains after stopping; startup must fail closed.
            docker("stop", "--time", "10", containers[1], timeout=30)
            restart = docker("run", "--name", containers[4], *common, "--user", "65533:65533", "--mount", mount,
                             "--env", "EVIDENCE_IMAGE_VALIDATOR_ENABLED=true",
                             "--env", "EVIDENCE_IMAGE_VALIDATOR_SOCKET=/run/evidence/validator.sock",
                             "--env", "EVIDENCE_IMAGE_VALIDATOR_ALLOWED_UID=65532", image, check=False)
            if restart.returncode != 1:
                raise RuntimeError("restart did not fail with the validator's expected exit code")
            state = json.loads(docker("inspect", containers[4]).stdout)[0]["State"]
            if state["Status"] != "exited" or state["ExitCode"] != 1 or state["Error"]:
                raise RuntimeError("restart did not start and exit as expected")
            docker("run", "--name", containers[5], "--label", owner, "--network", "none", "--user", "0:0", "--mount", mount,
                   "--entrypoint", "/bin/sh", image, "-c", "test -S /run/evidence/validator.sock")
            print("container UID, fixture, cgroup configuration and stale-socket checks passed")
    finally:
        # Only exact names generated by this run, and only objects bearing its
        # random ownership label, are eligible for cleanup.
        for name in containers:
            owned_cleanup("container", name, token)
        owned_cleanup("volume", volume, token)
        owned_cleanup("image", image, token)


def build_test_binary(binary, env):
    subprocess.run(["go", "test", "-tags=evidencecontainer,nomsgpack", "-c", "-o", str(binary), "./internal/billing"],
                   cwd=PLATFORM, env=env, check=True, timeout=600)


def initialize_volume(name, common, mount, image):
    # The init container retains CHOWN but not FOWNER: chmod while root is
    # still the owner, then transfer ownership to the isolated worker.
    docker("run", "--name", name, *common, "--cap-add", "CHOWN", "--user", "0:0",
           "--mount", mount, "--entrypoint", "/bin/sh", image, "-c",
           "chmod 0750 /run/evidence && chown 65533:65533 /run/evidence")


def check_peer_access(containers, owner, image, volume, binary):
    client(containers[2], owner, image, volume, binary, "65532:65533", "allowed")
    worker_ready(containers[1])
    client(containers[3], owner, image, volume, binary, "65531:65533", "wrong-caller")
    worker_ready(containers[1])
    client(containers[6], owner, image, volume, binary, "65532:65533", "allowed")


def worker_ready(name):
    until = time.monotonic() + 30
    while time.monotonic() < until:
        probe = docker("exec", name, "/bin/sh", "-c", "test -S /run/evidence/validator.sock", check=False)
        if probe.returncode == 0:
            return
        time.sleep(0.2)
    raise RuntimeError("worker did not create its socket")


def client(name, owner, image, volume, binary, user, mode):
    result = docker("run", "--name", name, "--label", owner, "--network", "none", "--read-only",
                    "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", user,
                    "--mount", "type=volume,src=" + volume + ",dst=/run/evidence,readonly",
                    "--mount", "type=bind,src=" + str(binary) + ",dst=/ci/test,readonly",
                    "--mount", "type=bind,src=" + str(HERE / "testdata") + ",dst=/ci/fixtures,readonly",
                    "--env", "EVIDENCE_CONTAINER_TEST_MODE=" + mode, "--entrypoint", "/ci/test", image,
                    "-test.v", "-test.run=^TestEvidenceValidatorContainer$", timeout=180)
    required = ["--- PASS: TestEvidenceValidatorContainer", "--- PASS: TestEvidenceValidatorContainer/png", "--- PASS: TestEvidenceValidatorContainer/progressive.jpg",
                "--- PASS: TestEvidenceValidatorContainer/animated.png", "--- PASS: TestEvidenceValidatorContainer/pdf/plain-compressed.pdf",
                "--- PASS: TestEvidenceValidatorContainer/pdf/truncated.pdf", "--- PASS: TestEvidenceValidatorContainer/pdf/user-password.pdf"]
    if "--- SKIP:" in result.stdout or any(line not in result.stdout for line in required):
        raise RuntimeError("container test did not execute every required case")


def owned_cleanup(kind, name, token):
    expression = '{{ index .Config.Labels "pinvou.ci.owner" }}' if kind in ("container", "image") else '{{ index .Labels "pinvou.ci.owner" }}'
    result = docker(kind, "inspect", "--format", expression, name, check=False)
    if result.returncode == 0 and result.stdout.strip() == token:
        docker(kind, "rm", name, check=False) if kind == "image" else docker(kind, "rm", "-f", name, check=False)


if __name__ == "__main__":
    main()

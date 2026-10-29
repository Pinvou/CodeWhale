# Ubuntu 22.04 runner image

Hosts run Ubuntu 26.04; this image runs the runner and native CI in Ubuntu
22.04 userspace to preserve the release compatibility baseline. Run one container per runner, with its own bind-mounted
directory under `/home/github-runner/ci-runners/` and matching absolute paths
inside and outside the container. This path consistency is required when the
runner uses the host Docker socket for service containers.

Build context inputs are deliberately excluded from Git:

- `artifacts/runner.tar.gz`: official actions/runner v2.338.2 Linux x64 archive.
- `artifacts/node.tar.xz`: official Node 24 Linux x64 archive. Pass its version
  and the SHA256 from that version's upstream SHASUMS256.txt as `NODE_VERSION`
  and `NODE_SHA256` build arguments.
- `artifacts/rustup-init`: official Linux x64 rustup installer.
- `artifacts/pipewire.tar.gz`: upstream PipeWire 1.0.7 source archive.
- `artifacts/google-linux-signing-key.pub`: Google's Linux package public key;
  only the pinned primary fingerprint is exported into the apt trust store.
- `extra-ca/*.crt`: optional administrator-verified corporate CA certificates.
- `ci-libpipewire-build.sh`: copy of `scripts/ci-libpipewire-build.sh`.

Runner, rustup, and PipeWire SHA256 values are checked in the Dockerfile. Node's
version and checksum are checked as supplied; obtain both from the official
release before building. Optional mirror arguments change download locations
only; cached archives do not bypass the checksum checks.

Run the preparation entrypoint once with `prepare`, register the runner using a
short-lived token on stdin, and run the normal entrypoint under a restartable
service. Keep registration credentials in the individual bind mount, never in
the image. Use host networking for runners that address published service ports,
and set resource limits on each runner container. The Docker socket grants host
administration capability, so these runners are reserved for trusted code.

Set one pool label (`pinvou3-heavy`, `pinvou3-medium`, or `pinvou3-light`) on each
runner and configure the corresponding repository variable with that label.
Do not give small runners the generic legacy label if queued jobs still use it.
Use `Self-hosted Runner Smoke` to verify all three pools after deployment.

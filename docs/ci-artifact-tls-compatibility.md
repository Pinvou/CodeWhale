# CI artifact TLS compatibility

The PR workflow limits the Node process of each `actions/upload-artifact@v7`
step to TLS 1.2. This is a transport compatibility measure for the observed
Azure Blob handshake failure, not a test timeout increase or a successful-upload
substitute. Test, retirement and upload deadlines retain their existing values.

The failing run completed its metadata request with HTTP 200, then stalled before
the Blob connection completed TLS negotiation. An anonymous, certificate-verified
control against the same advertised address succeeded with TLS 1.2 and timed out
with TLS 1.3. The actual Windows runner Node 24/Azure SDK also completed three
TLS-1.2 anonymous requests with verified certificates and the expected HTTP 401;
these controls do not prove an authenticated artifact was uploaded. Real CI
must still produce an upload byte count, finalization and an artifact ID.

Scope and security properties:

- Only artifact upload action processes receive `--tls-max-v1.2`. Business Node
  processes, the OS, Runner services and SSH keep their existing configuration.
- Existing `NODE_OPTIONS`, including diagnostic preloads and heap limits, are
  read from the preparation process's real environment and retained in a separate
  `PINVOU_CI_ARTIFACT_NODE_OPTIONS` value. Only the upload step applies that value;
  it is not a job-wide override. The default Node minimum remains TLS 1.2. An explicitly configured
  higher minimum is not reduced; an incompatible minimum/cap fails rather than
  silently weakening that policy.
- Certificate chains, hostnames, signed URLs and payload validation are unchanged.
  This does not enable TLS 1.0/1.1 or disable certificate verification.
- [Azure Storage supports TLS 1.2 and 1.3 and recommends a TLS 1.2 minimum](https://learn.microsoft.com/en-us/azure/storage/common/transport-layer-security-configure-minimum-version).

The failing device or upstream hop has not been uniquely identified. Remove this
cap after repeated default-negotiation controls and real uploads succeed on the
affected endpoints. Do not infer recovery from a green Job when an advisory
upload timed out: inspect its byte count, finalization and artifact ID.

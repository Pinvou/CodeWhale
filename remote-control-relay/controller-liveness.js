const TYPES = new Set([
  "controller_liveness",
  "controller_liveness_challenge_a",
  "encrypted_controller_liveness_a",
  "controller_liveness_attestation_request_a",
]);

export class ControllerLivenessHookError extends Error {
  constructor(code) {
    super(code);
    this.name = "ControllerLivenessHookError";
    this.code = code;
  }
}

export class ControllerLivenessHooks {
  handles(type) { return TYPES.has(type); }

  async handle() {
    throw new ControllerLivenessHookError("controller_liveness_not_enabled");
  }

  async onLeaseCreated() {}
  async onSocketClosed() { return Object.freeze({ retain_lease: false }); }
  async onLeaseTerminated() {}
  async expire() { return 0; }
}

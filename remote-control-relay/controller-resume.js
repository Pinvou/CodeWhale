const TYPES = new Set([
  "controller_resume_prepare",
  "encrypted_resume_confirm",
  "controller_resume_activation_ack",
  "controller_post_activation_liveness",
  "controller_resume_ready_ack",
]);

export class ControllerResumeHookError extends Error {
  constructor(code) {
    super(code);
    this.name = "ControllerResumeHookError";
    this.code = code;
  }
}

export class ControllerResumeHooks {
  handles(type) { return TYPES.has(type); }

  async handle() {
    throw new ControllerResumeHookError("controller_resume_not_enabled");
  }

  async onLeaseCreated() {}
  async onSocketClosed() { return Object.freeze({ retain_lease: false }); }
  async onLeaseTerminated() {}
  async expire() { return 0; }
}

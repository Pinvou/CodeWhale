import {
  RegistrationValidationContext,
  validateControllerMessage,
  validateControllerRegistrationMessage,
} from "./protocol-controller.js";
import { sealPairingCode } from "./pairing-code-sealer.js";

export class PairingRegistrationHandlerError extends Error {
  constructor(code, options) {
    super(code, options);
    this.name = "PairingRegistrationHandlerError";
    this.code = code;
  }
}

function fail(code, cause) {
  throw new PairingRegistrationHandlerError(code, cause ? { cause } : undefined);
}

export class PairingRegistrationHandler {
  #store;
  #relayEpoch;
  #authorizeEndpoint;

  constructor({ pairingStore, relayEpoch, authorizeEndpoint } = {}) {
    if (!pairingStore || typeof pairingStore.register !== "function"
      || typeof relayEpoch !== "string" || typeof authorizeEndpoint !== "function") {
      fail("pairing_registration_handler_invalid");
    }
    this.#store = pairingStore;
    this.#relayEpoch = relayEpoch;
    this.#authorizeEndpoint = authorizeEndpoint;
  }

  async register(message, authenticatedContext) {
    let request;
    try {
      request = validateControllerMessage(message, "B");
    } catch (error) {
      fail("pairing_registration_message_invalid", error);
    }
    if (request.type !== "desktop_pairing_register") fail("pairing_registration_message_invalid");
    try {
      await this.#authorizeEndpoint({
        endpointId: request.endpoint_id,
        generation: request.remote_control_generation,
        context: authenticatedContext,
      });
    } catch (error) {
      fail("pairing_endpoint_unauthorized", error);
    }
    const allocated = await this.#store.register({
      request,
      relayEpoch: this.#relayEpoch,
      sealCode: sealPairingCode,
    });
    try {
      const validation = new RegistrationValidationContext(request, allocated.result);
      return validateControllerRegistrationMessage(allocated.result, "relay", validation);
    } catch (error) {
      fail("pairing_registration_result_invalid", error);
    }
  }
}

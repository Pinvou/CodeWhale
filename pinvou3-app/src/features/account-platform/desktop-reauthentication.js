import { requirePersonalAccount } from './personal-contract.js';

export function desktopReauthenticationError(code) {
  const error = new Error(code);
  error.code = code;
  return error;
}

// One local UI ticket owns a native re-login. Credentials are accepted only by
// the existing login action; neither this record nor durable intent stores them.
export function createDesktopReauthenticationCoordinator({ client, getActor, onPrompt, onVerified, onRejected }) {
  let pending = null;
  let disposed = false;
  let sequence = 0;
  const current = ticket => Boolean(ticket) && !disposed && pending === ticket && ticket.client === client;
  const fail = (ticket, reason) => {
    if (!current(ticket)) return;
    pending = null;
    try { onRejected?.(ticket, reason); }
    catch { /* A failed UI callback cannot keep the old request alive. */ }
    finally { ticket.reject(reason); }
  };
  return {
    request(language) {
      if (disposed) return Promise.reject(desktopReauthenticationError('REAUTHENTICATION_EXPIRED'));
      if (pending) return Promise.reject(desktopReauthenticationError('REAUTHENTICATION_IN_PROGRESS'));
      let actor;
      try { actor = getActor(); }
      catch (reason) { return Promise.reject(reason); }
      if (typeof actor !== 'string' || !actor) return Promise.reject(desktopReauthenticationError('REAUTHENTICATION_UNAVAILABLE'));
      const ticket = { id: ++sequence, client, actor, language, inFlight: false };
      ticket.promise = new Promise((resolve, reject) => { ticket.resolve = resolve; ticket.reject = reject; });
      ticket.promise.catch(() => {}); // Its consumer still receives the rejection.
      pending = ticket;
      try { onPrompt(ticket); }
      catch (reason) { fail(ticket, reason); }
      return ticket.promise;
    },
    async authenticate(ticket, loginAction) {
      if (!current(ticket) || ticket.inFlight) throw desktopReauthenticationError('REAUTHENTICATION_EXPIRED');
      ticket.inFlight = true;
      try {
        await loginAction();
        if (!current(ticket)) throw desktopReauthenticationError('REAUTHENTICATION_EXPIRED');
        const verifiedEpoch = client.financeSessionEpoch;
        const profile = requirePersonalAccount(await client.bootstrap());
        if (!current(ticket) || verifiedEpoch !== client.financeSessionEpoch) throw desktopReauthenticationError('REAUTHENTICATION_EXPIRED');
        if (profile.user_id !== ticket.actor) throw desktopReauthenticationError('REAUTHENTICATION_ACTOR_MISMATCH');
        ticket.profile = profile;
        ticket.verifiedEpoch = verifiedEpoch;
        return profile;
      } catch (reason) {
        fail(ticket, reason);
        throw reason;
      } finally { ticket.inFlight = false; }
    },
    complete(ticket) {
      if (!current(ticket) || !ticket.profile || ticket.profile.user_id !== ticket.actor || ticket.verifiedEpoch !== client.financeSessionEpoch) {
        const reason = desktopReauthenticationError('REAUTHENTICATION_EXPIRED');
        fail(ticket, reason);
        throw reason;
      }
      try { onVerified(ticket, ticket.profile); }
      catch (reason) { fail(ticket, reason); throw reason; }
      if (!current(ticket)) throw desktopReauthenticationError('REAUTHENTICATION_EXPIRED');
      pending = null;
      ticket.resolve({ nativeReauthenticated: true, actor: ticket.actor, epoch: ticket.verifiedEpoch });
    },
    cancel(ticket) { fail(ticket, desktopReauthenticationError('REAUTHENTICATION_CANCELLED')); },
    fail,
    hasPending() { return pending !== null; },
    dispose() {
      const ticket = pending;
      if (ticket) fail(ticket, desktopReauthenticationError('REAUTHENTICATION_EXPIRED'));
      disposed = true;
    },
  };
}

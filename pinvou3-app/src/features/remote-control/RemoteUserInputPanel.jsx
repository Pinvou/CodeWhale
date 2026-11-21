import { useEffect, useRef, useState } from 'react';
import { QuestionChoiceCard } from '../conversation/QuestionChoiceCard.jsx';
import { remoteQuestionAnswers, remoteQuestionGroups } from './remote-user-input.mjs';

// Mounted with an exact lease/task key by ChatView. No question or decision
// state belongs to a different conversation, reconnect or subsequent task.
export default function RemoteUserInputPanel({ localSessionId, remoteRef, taskId, transport, t, onWaitingChange }) {
  const copy = t.remoteConversation;
  const [requests, setRequests] = useState([]);
  const [readError, setReadError] = useState(false);
  const [decisionError, setDecisionError] = useState(false);
  const [busy, setBusy] = useState(false);
  const [unknown, setUnknown] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const settled = useRef(new Set());
  const live = useRef(true);
  const inFlight = useRef(false);
  const originalDecision = useRef(null);

  useEffect(() => {
    live.current = true;
    return () => { live.current = false; };
  }, []);

  useEffect(() => {
    let cancelled = false;
    let timer;
    async function read() {
      try {
        const items = await transport.userInputList(localSessionId, remoteRef, taskId);
        if (cancelled) return;
        setRequests(items.filter(item => !settled.current.has(item.input_id)));
        setReadError(false);
        // One read at a time; a slow transport cannot accumulate polling work.
        timer = window.setTimeout(read, 1000);
      } catch {
        if (!cancelled) setReadError(true);
        // No blind retry against an older peer or a failed transport. The
        // explicit refresh is read-only and does not repeat an answer.
      }
    }
    void read();
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, [localSessionId, remoteRef, taskId, transport, refresh]);

  const waiting = requests.length > 0 || unknown;
  useEffect(() => {
    onWaitingChange(waiting);
    return () => onWaitingChange(false);
  }, [onWaitingChange, waiting]);

  async function respond(decision) {
    if (inFlight.current) return;
    inFlight.current = true;
    originalDecision.current = decision;
    setBusy(true);
    try {
      await transport.userInputRespond(localSessionId, remoteRef, taskId,
        decision.inputId, decision.cancel, decision.answers);
      if (!live.current) return;
      settled.current.add(decision.inputId);
      originalDecision.current = null;
      setUnknown(false);
      setDecisionError(false);
      setRequests(current => current.filter(item => item.input_id !== decision.inputId));
    } catch (failure) {
      if (!live.current) return;
      const uncertain = failure?.code === 'result_unknown' || failure?.resultUnknown === true;
      setUnknown(uncertain);
      setDecisionError(true);
      if (['user_input_stale', 'user_input_already_answered', 'task_session_binding_mismatch'].includes(failure?.code)) {
        settled.current.add(decision.inputId);
        setRequests(current => current.filter(item => item.input_id !== decision.inputId));
      }
      if (!uncertain) originalDecision.current = null;
    } finally {
      inFlight.current = false;
      if (live.current) setBusy(false);
    }
  }

  return (
    <section data-testid="remote-user-input" aria-label={copy.userInputTitle} className="mb-4 w-full max-w-[800px] mx-auto px-5 space-y-3">
      {requests.map(request => (
        <QuestionChoiceCard key={request.input_id} title={copy.userInputTitle}
          description={copy.userInputWaiting} questions={remoteQuestionGroups(request.questions)}
          submitting={busy || unknown} submitLabel={t.uiSubmit} cancelLabel={copy.userInputCancel}
          otherPlaceholder={t.uiToolRender.other} otherAnswerLabel={t.uiToolRender.other}
          onSubmit={groups => { void respond({ inputId: request.input_id, cancel: false, answers: remoteQuestionAnswers(groups) }); }}
          onCancel={() => { void respond({ inputId: request.input_id, cancel: true, answers: [] }); }} />
      ))}
      {(readError || decisionError) && <div role="alert" className="text-sm text-amber-700 dark:text-amber-300">
        {unknown ? copy.userInputUnknown : copy.userInputUnavailable}
        <button type="button" className="ml-2 underline" disabled={busy}
          onClick={() => {
            if (originalDecision.current) void respond(originalDecision.current);
            else setRefresh(value => value + 1);
          }}>{copy.retry}</button>
      </div>}
    </section>
  );
}

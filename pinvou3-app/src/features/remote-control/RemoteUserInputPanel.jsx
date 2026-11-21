import { useEffect, useRef, useState } from 'react';
import { QuestionChoiceCard } from '../conversation/QuestionChoiceCard.jsx';
import { advanceRemoteQuestionScope, remoteQuestionAnswers, remoteQuestionGroups } from './remote-user-input.mjs';

// Mounted with an exact lease/task key by ChatView. No question or decision
// state belongs to a different conversation, reconnect or subsequent task.
export default function RemoteUserInputPanel({
  localSessionId, remoteRef, taskId, pendingQuestionKey, retiredQuestionKey,
  transport, t, onWaitingChange, onDecisionPendingChange,
}) {
  const copy = t.remoteConversation;
  const [scope, setScope] = useState(() => ({ pendingKey: pendingQuestionKey, retiredKey: retiredQuestionKey, revision: 0 }));
  const nextScope = advanceRemoteQuestionScope(scope, pendingQuestionKey, retiredQuestionKey);
  if (nextScope !== scope) setScope(nextScope);
  const { revision } = nextScope;
  const [requestSnapshot, setRequestSnapshot] = useState(null);
  const [failedRead, setFailedRead] = useState(null);
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
    return () => {
      live.current = false;
      onDecisionPendingChange(false);
    };
  }, [onDecisionPendingChange]);

  useEffect(() => {
    if (!pendingQuestionKey) return;
    let cancelled = false;
    let timer;
    async function read() {
      try {
        const items = await transport.userInputList(localSessionId, remoteRef, taskId);
        if (cancelled) return;
        setRequestSnapshot({
          revision,
          requests: items.filter(item => !settled.current.has(item.input_id)),
        });
        setFailedRead(null);
        // One read at a time; a slow transport cannot accumulate polling work.
        timer = window.setTimeout(read, 1000);
      } catch {
        if (!cancelled) setFailedRead({ pendingKey: pendingQuestionKey, revision });
        // No blind retry against an older peer or a failed transport. The
        // explicit refresh is read-only and does not repeat an answer.
      }
    }
    void read();
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, [localSessionId, remoteRef, taskId, pendingQuestionKey, revision, transport, refresh]);

  const requests = pendingQuestionKey && requestSnapshot?.revision === revision
    ? requestSnapshot.requests : [];
  const readError = failedRead?.pendingKey === pendingQuestionKey && failedRead.revision === revision;
  const waiting = requests.length > 0 || busy || unknown;
  useEffect(() => {
    onWaitingChange(waiting);
    return () => onWaitingChange(false);
  }, [onWaitingChange, waiting]);

  function retireRequest(inputId) {
    settled.current.add(inputId);
    setRequestSnapshot(current => current && {
      ...current,
      requests: current.requests.filter(item => item.input_id !== inputId),
    });
  }

  async function respond(decision) {
    if (inFlight.current) return;
    inFlight.current = true;
    originalDecision.current = decision;
    // A tool completion can arrive before the answer acknowledgement. Keep this
    // decision mounted for the same task and lease until its result is known.
    onDecisionPendingChange(true);
    setBusy(true);
    try {
      await transport.userInputRespond(localSessionId, remoteRef, taskId,
        decision.inputId, decision.cancel, decision.answers);
      if (!live.current) return;
      retireRequest(decision.inputId);
      originalDecision.current = null;
      onDecisionPendingChange(false);
      setUnknown(false);
      setDecisionError(false);
    } catch (failure) {
      if (!live.current) return;
      const uncertain = failure?.code === 'result_unknown' || failure?.resultUnknown === true;
      setUnknown(uncertain);
      setDecisionError(true);
      if (['user_input_stale', 'user_input_already_answered', 'task_session_binding_mismatch'].includes(failure?.code)) {
        retireRequest(decision.inputId);
      }
      if (!uncertain) {
        originalDecision.current = null;
        onDecisionPendingChange(false);
      }
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

import { useEffect, useRef } from 'react';
import { tauriEvents, listenManualModelSelection } from '../../platform/tauri/client.js';
import { subscribeRuntimeTasks } from './runtime-task-bus.js';
import { createTaskEventBuffer } from './task-event-buffer.js';

/** Install shell listeners before loading the controller; retain event-time selection. */
export function useCmsdEventBuffer({ bridge, snapshot }) {
  const latest = useRef(snapshot);
  latest.current = snapshot;
  const buffer = useRef(null);
  buffer.current ??= createTaskEventBuffer();
  useEffect(() => {
    if (!bridge.available) return undefined;
    let disposed = false;
    const unlisten = [];
    const publish = (method, payload) => {
      const state = bridge.state?.getMany?.(['sessions', 'models']) || latest.current;
      buffer.current.publish(method, payload, { activeSessionId: state?.activeSessionId,
        currentSessionModelId: state?.currentSessionModelId, activeModelId: state?.activeModelId });
    };
    for (const [event, method] of [['chat:tool_start', 'toolStarted'], ['chat:tool_end', 'toolEnded'], ['session:manual_model_selection', 'manualSelection']]) {
      void tauriEvents.listen(event, message => publish(method, method === 'manualSelection'
        ? { sessionId: message.payload?.session_id } : message.payload)).then(stop => {
        if (disposed) stop(); else unlisten.push(stop);
      }).catch(error => console.warn('[megacube] task listener unavailable', error));
    }
    unlisten.push(listenManualModelSelection(payload => publish('manualSelection', payload)));
    unlisten.push(subscribeRuntimeTasks(task => publish('runtimeTask', task)));
    return () => { disposed = true; for (const stop of unlisten) stop(); };
  }, [bridge]);
  return buffer.current;
}

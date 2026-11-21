import { useEffect } from 'react';
import { createPortal } from 'react-dom';
import { useDeviceManagement } from './use-device-management.js';
import { CmsdTaskModal } from './CmsdTaskModal.jsx';
import { useCmsdSwitchFollow } from './use-cmsd-switch-follow.js';

/** Deferred task presentation/controller; the shell already buffers events. */
export function CmsdTaskRuntime(props) {
  const watch = useCmsdSwitchFollow(props);
  const device = useDeviceManagement();
  useEffect(() => { props.onDeviceChange(device); }, [device, props.onDeviceChange]);
  return (watch.activeTask || watch.settledTask) ? createPortal(
    <CmsdTaskModal task={watch.activeTask} settled={watch.settledTask}
      onDismiss={watch.dismissSettled} copy={props.copy} />, document.body,
  ) : null;
}

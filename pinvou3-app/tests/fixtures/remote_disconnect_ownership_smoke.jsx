import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { dictZh } from '../../src/shared/i18n/zh.js';
import { useRemoteConversationVisibility } from '../../src/features/remote-control/useRemoteConversationVisibility.js';

// Real mounted consumers and dialog owner, with controlled host IPC promises.
// This fixture does not execute native connection or transport code.
const fixture = window.disconnectFixture = { requests: [], copy: dictZh.controllerConnection };
window.TauriBridge = {
  available: true,
  desktopRemoteControl: {
    controllerStatus: async () => ({}),
    controllerDisconnect: () => new Promise((resolve, reject) => {
      fixture.requests.push({ resolve, reject });
    }),
    getRemoteProxyLoadStatus: async () => null,
    listRemoteProxyArchiveCandidates: async () => [],
    listRemoteProxyArchives: async () => [],
  },
};
const { default: Panel } = await import('../../src/features/remote-control/RemoteControllerPanel.jsx');
const { default: Shell } = await import('../../src/features/remote-control/RemoteConnectionShell.jsx');

function App() {
  const [connection, setConnection] = useState({ status: 'connected', connectionId: 'connection-1' });
  const { open, setOpen, close } = useRemoteConversationVisibility();
  fixture.setConnection = setConnection;
  const bs = { controllerConnection: connection };
  return <>
    <Panel t={dictZh} bs={bs} conversationOpen={open}
      onConversationOpen={() => Promise.resolve(false).then(() => setOpen(true))}
      onConversationClose={close} />
    {open && <div role="dialog">
      <Shell key={connection.connectionId} t={dictZh} bs={bs} onClose={close}
        onRequestDisconnect={async () => {
          await window.TauriBridge.desktopRemoteControl.controllerDisconnect();
          close();
        }} />
    </div>}
  </>;
}
createRoot(document.getElementById('root')).render(<App />);

import React, { useState } from 'react';
import { flushSync } from 'react-dom';
import { createRoot } from 'react-dom/client';
import { PasswordInput } from '../../src/features/account-platform/PasswordInput.jsx';

function Fixture() {
  const [value, setValue] = useState('');
  const [resetKey, setResetKey] = useState(0);
  const [authToken, setAuthToken] = useState('guest');
  const [mode, setMode] = useState('password');
  const [open, setOpen] = useState(true);

  globalThis.__passwordFixture = {
    setValue(next) {
      flushSync(() => setValue(String(next)));
    },
    reset() {
      flushSync(() => setResetKey(current => current + 1));
      const output = document.querySelector('[data-password-reveal-output]');
      return { hidden: output.hidden, text: output.textContent };
    },
    semanticReset(kind) {
      flushSync(() => {
        if (kind === 'auth') setAuthToken(current => current === 'guest' ? 'error' : 'guest');
        if (kind === 'mode') setMode(current => current === 'password' ? 'forgot' : 'password');
        if (kind === 'open') setOpen(current => !current);
      });
      const output = document.querySelector('[data-password-reveal-output]');
      return { hidden: output.hidden, text: output.textContent };
    },
  };

  return (
    <main className="m-8 w-[360px]">
      <form onSubmit={(event) => {
        event.preventDefault();
        setResetKey(current => current + 1);
      }}>
        <input name="username" autoComplete="username" value="fixture-user" readOnly hidden />
        <PasswordInput
          value={value}
          onValueChange={setValue}
          onBlur={() => {}}
          resetKey={`${authToken}:${mode}:${open ? 'open' : 'closed'}:${resetKey}`}
          showLabel="Show password"
          hideLabel="Hide password"
          placeholder="Password"
          ariaLabel="Password"
          autoComplete="current-password"
          controlTheme="bg-white border-black/10 text-[#1F1F1F] placeholder:text-[#9AA0A6]"
        />
        <button id="submit-password" type="submit">Submit</button>
      </form>
      <button id="outside-control" type="button">Outside</button>
    </main>
  );
}

globalThis.__passwordEvents = [];
for (const type of ['beforeinput', 'input', 'compositionstart', 'compositionupdate', 'compositionend']) {
  document.addEventListener(type, (event) => {
    if (!event.target.matches?.('[data-password-input] input')) return;
    globalThis.__passwordEvents.push({
      type,
      inputType: event.inputType || '',
      data: event.data ?? null,
      trusted: event.isTrusted,
      value: event.target.value,
      start: event.target.selectionStart,
      end: event.target.selectionEnd,
    });
  }, true);
}

createRoot(document.getElementById('root')).render(<Fixture />);

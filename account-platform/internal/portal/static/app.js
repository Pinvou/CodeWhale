(async function () {
  const status = document.getElementById('status');
  const login = document.getElementById('login');
  const workspace = document.getElementById('workspace');
  const profile = document.getElementById('profile');
  const response = await fetch('/api/session', { cache: 'no-store', credentials: 'same-origin' });
  if (!response.ok) {
    status.textContent = '请先登录账号平台。';
    login.hidden = false;
    return;
  }
  const session = await response.json();
  const account = await fetch('/api/account/v1/account/bootstrap', { method: 'POST', headers: { 'X-CSRF-Token': session.csrf_token }, credentials: 'same-origin' });
  status.textContent = account.ok ? '账号平台已连接。' : '账号初始化暂不可用。';
  workspace.hidden = false;
  if (account.ok) profile.textContent = JSON.stringify(await account.json(), null, 2);
  document.getElementById('logout').addEventListener('click', async () => {
    await fetch('/api/logout', { method: 'POST', headers: { 'X-CSRF-Token': session.csrf_token }, credentials: 'same-origin' });
    location.reload();
  });
}());

import { state, api, esc, date, toast, projectName } from './api.js';

import {brand, heading, empty, layout} from './console-layout.js';
import {memoryList, memoryEditor, operationNames, processingStatus, processingLabel} from './memory-views.js';
import {projectsPage, agentList, newAgent, agentDetail} from './connection-views.js';

// The hint only selects the expected login in an embedded browser. All owner
// access is still authorized by MemoSphere's verified session and CSRF token.
const embedded = new URLSearchParams(location.search).get('embedded') === '1';
function embeddedOwner() {
  if (!embedded) return '';
  const [route, query] = location.hash.split('?');
  const hint = new URLSearchParams(query || '').get('owner');
  if (hint !== null) {
    sessionStorage.setItem('ms.embeddedOwner', hint.trim().toLowerCase().slice(0, 254));
    history.replaceState(null, '', location.pathname + location.search + route);
  }
  return sessionStorage.getItem('ms.embeddedOwner') || '';
}
const expectedOwner = embeddedOwner();
function matchesOwner(session) {
  return !embedded || (expectedOwner && session.user.email.toLowerCase() === expectedOwner);
}

const root = document.querySelector('#app');
let generation = 0;
const actions = { 'memory.organized': '自动整理了记忆', 'account.created': '创建了记忆空间', 'project.created': '创建了项目', 'memory.created': '新增了一条记忆', 'memory.updated': '修改了一条记忆', 'memory.deleted': '删除了记忆及其历史', 'agent.created': '创建了 Agent 连接', 'agent.permissions_updated': '调整了 Agent 权限', 'agent.revoked': '撤销了 Agent 授权', 'agent.setup_created': '生成了一次性接入码', 'agent.setup_completed': 'Agent 兑换了接入凭证', 'agent.key_rotated': '更换了 Agent Key' };

const resendDeadlines = new Map();
let resendTimer;

function updateSendButton(form) {
  clearTimeout(resendTimer);
  if (!form.isConnected) return;
  const button = form.querySelector('[data-action="send-code"]');
  const key = form.dataset.purpose + '/' + form.elements.email.value.trim().toLowerCase();
  const seconds = Math.ceil(((resendDeadlines.get(key) || 0) - Date.now()) / 1000);
  button.disabled = form.dataset.sending === 'true' || seconds > 0;
  button.textContent = form.dataset.sending === 'true' ? '正在发送…'
    : seconds > 0 ? seconds + ' 秒后重发' : form.dataset.sent === key ? '重新发送' : '发送验证码';
  if (seconds > 0) resendTimer = setTimeout(() => updateSendButton(form), 1000);
}

async function sendCode(form) {
  if (!form.elements.email.reportValidity() || !form.elements.password.reportValidity()) return;
  const { purpose } = form.dataset;
  const email = form.elements.email.value.trim().toLowerCase();
  const key = purpose + '/' + email;
  const hint = form.querySelector('[role="status"]');
  const error = form.querySelector('.error');
  form.dataset.sending = 'true';
  error.textContent = '';
  updateSendButton(form);
  try {
    const result = await api('/auth/code', { method: 'POST', body: JSON.stringify({purpose, email, password: form.elements.password.value}) });
    resendDeadlines.set(key, Date.now() + result.resend_after * 1000);
    if (form.isConnected && form.elements.email.value.trim().toLowerCase() === email) {
      form.dataset.sent = key;
      hint.textContent = '验证码已发送至 ' + email + '，10 分钟内有效。';
      form.elements.code.value = '';
      form.elements.code.focus();
    }
  } catch (err) {
    if (err.retryAfter) resendDeadlines.set(key, Date.now() + err.retryAfter * 1000);
    if (form.isConnected && form.elements.email.value.trim().toLowerCase() === email) error.textContent = err.message;
  } finally {
    form.dataset.sending = 'false';
    if (form.isConnected) updateSendButton(form);
  }
}

function auth(register = false) {
  generation++;
  document.querySelector('#toast').hidden = true;
  root.innerHTML = `<main id="main-content" class="auth-page"><aside class="auth-side">${brand}<div><h2 class="auth-quote">个人记忆控制台</h2><p>查看记忆与来源，管理客户端的访问权限。</p></div></aside>
    <section class="auth-main"><div class="auth-form"><h1>${register ? '注册 MemoSphere' : '登录 MemoSphere'}</h1>
    <form id="auth-form" class="form-stack" data-purpose="${register ? 'register' : 'login'}">
      <label>邮箱地址<input name="email" type="email" autocomplete="email" required maxlength="254" placeholder="you@example.com"></label>
      <label>密码<input name="password" type="password" autocomplete="${register ? 'new-password' : 'current-password'}" minlength="${register ? '10' : '1'}" maxlength="256" required placeholder="${register ? '至少 10 个字符' : '输入密码'}"></label>
      <div><label for="email-code">邮箱验证码</label><div class="verification-field"><input id="email-code" name="code" type="text" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required placeholder="6 位数字" aria-describedby="mail-status"><button type="button" data-action="send-code">发送验证码</button></div><small id="mail-status" role="status" aria-live="polite"></small></div>
      <p class="error" role="alert" id="form-error"></p><button class="primary" type="submit">${register ? '注册并进入控制台' : '登录控制台'}</button></form>
    <p class="auth-switch">${register ? '已有账号？' : '还没有账号？'} <a href="${register ? '#/login' : '#/register'}">${register ? '去登录' : '用邮箱注册'}</a></p></div></section></main>`;
  const form = document.querySelector('#auth-form');
  if (embedded) {
    if (!expectedOwner) {
      root.textContent = '请从客户端重新打开记忆管理。';
      return;
    }
    form.elements.email.value = expectedOwner;
    form.elements.email.readOnly = true;
  }
  updateSendButton(form);
}

async function activityPage() {
  const { items } = await api('/activity');
  return `${heading('活动记录', '查看记忆修改和授权变更，重要操作都有记录。')}<section class="timeline">${items.map(e => `<article class="event"><span class="dot" aria-hidden="true"></span><div><p>${esc(actions[e.action] || '记录了操作')}</p>${e.details.name ? `<small>${esc(e.details.name)}</small>` : ''}${e.details.revision ? `<small>版本 v${e.details.revision}${e.details.operation ? ' · ' + esc(operationNames[e.details.operation] || e.details.operation) : ''}</small>` : ''}</div><time class="row-time">${date(e.created_at)}</time></article>`).join('') || empty('暂时没有活动', '记忆修改和 Agent 授权变更会显示在这里。')}</section>`;
}

async function conversationPage() {
  const { items } = await api('/conversations');
  return `${heading('对话记录', '保留已授权会话的原始消息，供后续整理与回溯。')}<section class="notice"><div><h3>原始对话与长期记忆分开保存</h3><p>${state.session.automatic_extraction ? '已授权的新消息会自动提取和更新记忆，无需逐条确认。原文独立保留，错误结论和不确定记忆不会作为当前事实提供给助手。' : '自动整理尚未启用。原文独立保留，你添加的记忆可以正常使用。'}</p></div></section><div class="section-title"><h3>最近同步</h3><p>最多显示最近 100 条</p></div>${processingStatus()}<section class="table-list">${items.length ? items.map(m => `<details class="conversation"><summary><span class="pill">${esc({ user: '用户', assistant: '助手', tool: '工具结果' }[m.role] || m.role)}</span> <span class="pill">${processingLabel(m)}</span> ${esc(m.agent_name || 'Agent')} · ${esc(projectName(m.project_id))} <small>${date(m.recorded_at)}</small></summary><p>${esc(m.text)}</p></details>`).join('') : empty('等待第一次同步', '完成 Agent 接入后，在已绑定的项目里开始对话。', '<a class="button" href="#/agents">管理 Agent 连接 ↗</a>')}</section>`;
}

async function render() {
  const turn = ++generation;
  if (!state.session) return auth(location.hash === '#/register');
  layout(root); const main = document.querySelector('#main-content');
  const path = location.hash.replace(/^#\//, '') || 'memories';
  try {
    let html;
    if (path === 'memories') html = await memoryList();
    else if (path.startsWith('memory/')) html = await memoryEditor(path.split('/')[1]);
    else if (path === 'projects') html = await projectsPage();
    else if (path === 'agents') html = await agentList();
    else if (path === 'agents/new') html = await newAgent();
    else if (path.startsWith('agents/')) html = await agentDetail(path.split('/')[1]);
    else if (path === 'activity') html = await activityPage();
    else if (path === 'conversations') html = await conversationPage();
    else html = empty('没有找到这个页面', '返回记忆空间，继续查看你的记录。', '<a class="button primary" href="#/memories">回到我的记忆</a>');
    if (turn === generation) main.innerHTML = html;
  } catch (error) {
    if (!state.session) return auth();
    if (turn === generation) main.innerHTML = empty('暂时无法打开', esc(error.message), '<button data-action="refresh">重试</button>');
  }
}

async function refresh() {
  const current = await api('/session');
  if (!matchesOwner(current)) {
    state.session = null;
    state.projects = [];
    throw new Error('请验证当前账号的邮箱。');
  }
  state.session = current;
  state.projects = (await api('/projects')).items;
}
function permissions(form) {
  const data = new FormData(form);
  return { projects: data.getAll('projects'), ...Object.fromEntries(['global_access', 'sensitive_access', 'read_access', 'write_access'].map(k => [k, data.has(k)])) };
}

root.addEventListener('submit', async event => {
  event.preventDefault(); const form = event.target; const data = new FormData(form);
  const button = form.querySelector('button[type=submit]'); const error = form.querySelector('.error') || document.querySelector('#form-error');
  if (button?.disabled) return;
  if (button) button.disabled = true;
  if (error) error.textContent = '';
  try {
    if (form.id === 'auth-form') {
      await api(`/auth/${form.dataset.purpose}`, { method: 'POST', body: JSON.stringify(Object.fromEntries(data)) });
      await refresh(); location.hash = '#/memories'; await render();
    } else if (form.id === 'search-form') {
      state.query = data.get('q'); state.project = data.get('project'); state.memoryState = data.get('memory_state'); state.offset = 0; await render();
    } else if (form.id === 'project-form') {
      await api('/projects', { method: 'POST', body: JSON.stringify({ name: data.get('name') }) });
      await refresh(); toast('项目已创建');
      if (form.dataset.next) location.hash = '#/' + form.dataset.next;
      await render();
    } else if (form.id === 'memory-form') {
      const payload = { ...Object.fromEntries(data), tags: String(data.get('tags')).split(/[,，]/).map(s => s.trim()).filter(Boolean), sensitive: data.has('sensitive'), revision: Number(form.dataset.revision) };
      await api(form.dataset.id ? `/memories/${form.dataset.id}` : '/memories', { method: form.dataset.id ? 'PUT' : 'POST', body: JSON.stringify(payload) });
      await refresh(); toast('记忆已保存'); location.hash = '#/memories'; await render();
    } else if (form.id === 'agent-form') {
      const result = await api('/agents', { method: 'POST', body: JSON.stringify({ ...Object.fromEntries(data), ...permissions(form) }) });
      await refresh(); location.hash = '#/agents/' + result.id; toast('独立连接已创建'); await render();
    } else if (form.id === 'permissions-form') {
      await api(`/agents/${form.dataset.id}`, { method: 'PATCH', body: JSON.stringify(permissions(form)) });
      toast('权限已更新'); await render();
    }
  } catch (exception) {
    if (error?.isConnected) error.textContent = exception.message;
    else toast(exception.message);
  } finally { if (button?.isConnected) button.disabled = false; }
});

root.addEventListener('click', async event => {
  const target = event.target.closest('[data-action]'); if (!target || target.disabled) return;
  const action = target.dataset.action, id = target.dataset.id;
  try {
    if (action === 'logout') { await api('/auth/logout', { method: 'POST' }); state.session = null; auth(); }
    if (action === 'refresh') { await refresh(); await render(); }
    if (action === 'send-code') { await sendCode(target.form); return; }
    if (action === 'reset-search') { state.query = ''; state.project = ''; state.memoryState = 'active'; state.offset = 0; await render(); }
    if (action === 'next' || action === 'previous') { state.offset += action === 'next' ? 50 : -50; await render(); }
    if (action === 'project-memories') { state.project = id; state.query = ''; state.offset = 0; location.hash = '#/memories'; await render(); }
    if (action === 'delete-memory') { const result = await api(`/memories/${id}`, { method: 'DELETE' }); await refresh(); toast(result.deleted?.memories > 1 ? '已删除记忆、关联来源及共享同一证据片段的记忆' : '记忆、历史及关联来源已清理'); location.hash = '#/memories'; await render(); }
    if (action === 'revoke') { target.disabled = true; await api(`/agents/${id}`, { method: 'DELETE' }); await refresh(); toast('授权已撤销，后续请求将被拒绝'); await render(); }
    if (action === 'setup') {
      target.disabled = true;
      const setup = await api(`/agents/${id}/setup`, { method: 'POST' });
      const generic = target.dataset.kind === 'other';
      document.querySelector('#setup-result').innerHTML = generic
        ? '<div class="secret"><p>接入码</p><pre class="code-box" id="setup-command"></pre><button data-action="copy-command">复制接入码</button><p>客户端向下方地址 POST 一个含 code 字段的 JSON 对象，取得 key 后使用 Authorization: Bearer 认证。</p><pre class="code-box" id="exchange-url"></pre></div>'
        : '<div class="secret"><p>在 MemoSphere 安装目录执行，接入码 10 分钟内有效。</p><pre class="code-box" id="setup-command"></pre><button data-action="copy-command">复制命令</button></div>';
      document.querySelector('#setup-command').textContent = generic ? setup.code : setup.command;
      if (generic) document.querySelector('#exchange-url').textContent = setup.exchange_url;
      target.disabled = false;
    }
    if (action === 'copy-command') { await navigator.clipboard.writeText(document.querySelector('#setup-command').textContent); toast('已复制'); }
    if (action === 'rotate') {
      target.disabled = true; const { key } = await api(`/agents/${id}/rotate`, { method: 'POST' });
      document.querySelector('#key-result').innerHTML = '<div class="secret"><p>旧 Key 已失效。请立即保存新 Key，离开页面后不会再次显示。</p><pre class="code-box" id="new-key"></pre><button data-action="copy-key">复制新 Key</button></div>';
      document.querySelector('#new-key').textContent = key; target.disabled = false;
    }
    if (action === 'copy-key') { await navigator.clipboard.writeText(document.querySelector('#new-key').textContent); toast('Key 已复制'); }
  } catch (error) { target.disabled = false; toast(error.message); }
});
root.addEventListener('input', event => {
  const form = event.target.form;
  if (form?.id === 'auth-form' && event.target.name === 'email') {
    form.elements.code.value = '';
    form.querySelector('[role="status"]').textContent = '';
    form.querySelector('.error').textContent = '';
    updateSendButton(form);
  }
});
root.addEventListener('change', event => {
  if (event.target.name === 'kind') {
    const field = event.target.form.elements.name;
    if (['我的 Codex', '我的 Kimi Code', '我的客户端'].includes(field.value)) field.value = `我的 ${{codex: 'Codex', kimi: 'Kimi Code', other: '客户端'}[event.target.value]}`;
  }
});
window.addEventListener('hashchange', render);
try { await refresh(); await render(); } catch { auth(location.hash === '#/register'); }

// 仅在列表中更新完成状态，不打断正在编辑或键盘操作的用户。
let checkingProgress = false;
setInterval(async () => {
  if (checkingProgress || !state.session || document.visibilityState !== 'visible' ||
      !['#/memories', '#/conversations', ''].includes(location.hash) ||
      document.activeElement?.matches('input,textarea,select,button,a') || window.getSelection()?.toString()) return;
  checkingProgress = true;
  try {
    const previous = JSON.stringify([state.session.processing, state.session.memory_epoch, state.session.counts]);
    const current = await api('/session');
    if (!matchesOwner(current)) { state.session = null; state.projects = []; auth(); return; }
    if (previous !== JSON.stringify([current.processing, current.memory_epoch, current.counts])) {
      state.session = current;
      const position = window.scrollY;
      await render(); window.scrollTo(0, position);
    }
  } catch {} finally { checkingProgress = false; }
}, 5000);

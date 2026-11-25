export const state = { session: null, projects: [], route: '', query: '', project: '', offset: 0, memoryState: 'active' };

export async function api(path, options = {}) {
  const headers = { ...(options.body ? { 'Content-Type': 'application/json' } : {}), ...options.headers };
  if (state.session?.csrf) headers['X-CSRF-Token'] = state.session.csrf;
  const response = await fetch(`/api${path}`, { ...options, headers, credentials: 'same-origin' });
  let data;
  try { data = await response.json(); }
  catch { throw new Error('服务没有返回有效结果，请稍后重试'); }
  if (!response.ok) {
    if (response.status === 401) state.session = null;
    const error = new Error(data.error || '操作未完成，请重试');
    error.retryAfter = Number(data.resend_after) || 0;
    throw error;
  }
  return data;
}

export const esc = value => String(value ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
export function date(value) {
  return value ? new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }).format(new Date(value * 1000)) : '尚无记录';
}
export function toast(message) {
  const node = document.querySelector('#toast');
  node.textContent = message; node.hidden = false;
  clearTimeout(toast.timer); toast.timer = setTimeout(() => { node.hidden = true; }, 3500);
}
export function projectName(id) { return state.projects.find(p => p.id === id)?.name || (id ? '项目记忆' : '个人全局'); }
export function projectOptions(selected = '', includeGlobal = true) {
  return `${includeGlobal ? '<option value="">个人全局</option>' : ''}${state.projects.map(p => `<option value="${esc(p.id)}" ${p.id === selected ? 'selected' : ''}>${esc(p.name)}</option>`).join('')}`;
}

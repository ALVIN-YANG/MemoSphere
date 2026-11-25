import {state, esc} from './api.js';

const navigation = [['memories', '◈', '记忆'], ['projects', '▦', '项目'], ['agents', '◇', 'Agent 授权'], ['conversations', '≡', '对话记录'], ['activity', '◷', '活动']];
export const brand = `<a href="#/memories" class="brand"><span class="brand-mark" aria-hidden="true"></span><span class="brand-name" lang="en"><span>Memory</span><span>Sphere</span></span></a>`;
export function heading(title, subtitle, action = '') { return `<div class="page-heading"><div><div class="eyebrow" lang="en">PERSONAL MEMORY</div><h1>${title}</h1><p>${subtitle}</p></div>${action}</div>`; }
export function empty(title, description, action = '') { return `<div class="empty"><div class="empty-symbol" aria-hidden="true">◈</div><h2>${title}</h2><p>${description}</p>${action}</div>`; }
export function layout(root) {
  const section = location.hash.startsWith('#/memory/') ? 'memories' : location.hash.split('/')[1] || 'memories';
  root.innerHTML = `<div class="shell"><aside class="sidebar">${brand}<div><p class="workspace-label">我的空间</p><nav class="nav" aria-label="主要导航">${navigation.map(([id, icon, title]) => `<a href="#/${id}" class="${section === id ? 'active' : ''}" ${section === id ? 'aria-current="page"' : ''}><span class="nav-icon" aria-hidden="true">${icon}</span>${title}</a>`).join('')}</nav></div><div class="sidebar-foot"><strong>记忆由你掌控</strong><p class="account-email muted">${esc(state.session.user.email)}</p><a href="/api/export" class="button quiet">导出我的记忆 ↗</a></div></aside><div class="workspace"><header class="topbar"><div class="crumb">个人空间 <b>/</b> ${navigation.find(n => n[0] === section)?.[2] || '记忆'}</div><div class="space-indicator"><span class="dot" aria-hidden="true"></span>仅你可管理 <button class="quiet" data-action="logout">退出</button></div></header><main id="main-content" class="content" tabindex="-1"><p class="muted">正在读取…</p></main></div></div>`;
}

import {state, api, esc, date, projectName, projectOptions} from './api.js';
import {heading, empty} from './console-layout.js';

export const operationNames = { RECLASSIFY: "调整分类", ADD: '自动新增', UPDATE: '情况变化', CORRECT: '事实纠错', MERGE: '补充合并', UNCERTAIN: '暂不确定', IGNORE: '忽略', USER_EDIT: '手动编辑' };
const dispositions = { current: '当前内容', historical: '历史事实', invalidated: '已作废', superseded: '已替换', uncertain: '暂不确定' };
function counts() {
  const c = state.session.counts;
  return `<div class="counts"><span><strong>${c.memories}</strong>条记忆</span><span><strong>${c.projects}</strong>个项目</span><span><strong>${c.agents}</strong>个授权 Agent</span></div>`;
}

export function processingStatus() {
  const p = state.session.processing || {};
  if (!state.session.automatic_extraction) return '';
  return `<div class="processing-status" role="status"><span><span class="dot" aria-hidden="true"></span>自动整理已启用</span><small>已处理 ${p.done || 0} 条 · 等待 ${p.pending || 0} 条 · 处理中 ${p.running || 0} 条${p.failed ? ' · ' + p.failed + ' 条稍后重试' : ''}${p.skipped ? ' · ' + p.skipped + ' 条未整理或原有归档' : ''}</small><small>模型调用 ${state.session.model_usage?.calls || 0} 次</small></div>`;
}
export async function memoryList() {
  const params = new URLSearchParams({ q: state.query, project: state.project, offset: state.offset, state: state.memoryState });
  const data = await api(`/memories?${params}`);
  const filtered = Boolean(state.query || state.project || state.memoryState !== 'active');
  return `${heading('我的记忆', '那些值得记住的事，都有迹可循。', '<a class="button primary" href="#/memory/new">＋ 新增记忆</a>')}${counts()}${processingStatus()}
    ${!state.session.counts.agents ? `<section class="notice"><div><h3>让下一个助手，接着了解你</h3><p>授权一个 MCP 客户端读取或写入你的记忆。</p></div><a class="button primary" href="#/agents/new">连接第一个 Agent ↗</a></section>` : ''}
    <form class="toolbar" id="search-form"><label class="search"><input type="search" name="q" value="${esc(state.query)}" aria-label="搜索记忆" placeholder="搜索经历、偏好、项目决策…"></label><select name="project" aria-label="按项目筛选"><option value="">全部范围</option><option value="global" ${state.project === 'global' ? 'selected' : ''}>个人全局</option>${state.projects.map(p => `<option value="${p.id}" ${p.id === state.project ? 'selected' : ''}>${esc(p.name)}</option>`).join('')}</select><select name="memory_state" aria-label="按记忆状态筛选">${[['active','当前有效'],['uncertain','暂不确定'],['all','全部状态']].map(([v,t]) => `<option value="${v}" ${state.memoryState === v ? 'selected' : ''}>${t}</option>`).join('')}</select><button type="submit">搜索</button></form>
    <div class="section-title"><h3>${filtered ? '筛选结果' : '最近更新'}</h3><p>${data.total} 条记忆</p></div><section class="memory-list" aria-label="记忆列表">${data.items.length ? data.items.map(m => `<article class="memory-row"><span class="memory-symbol" aria-hidden="true">${m.project_id ? '◇' : '◈'}</span><div><a class="memory-title" href="#/memory/${m.id}">${esc(m.text)}</a><div class="memory-meta"><span class="pill">${esc(m.category)}</span>${m.sensitive ? '<span class="pill private">敏感 · 需单独授权</span>' : ''}<span>${esc(projectName(m.project_id))}</span>${m.tags.map(t => `<span class="tag">${esc(t)}</span>`).join('')}<span>${m.origin === 'automatic' ? (operationNames[m.last_operation] || '自动整理') : '你添加的记忆'} · v${m.revision}</span>${m.state === 'uncertain' ? '<span class="pill">暂不确定，不用于回答</span>' : ''}</div></div><time class="row-time">${date(m.updated_at)}</time></article>`).join('') : empty(filtered ? '没有找到相关记忆' : '从一条重要的记忆开始', filtered ? '换个关键词，或查看全部范围。' : '可以先记下你的偏好或项目约定，授权后的助手就能使用。', filtered ? '<button data-action="reset-search">查看全部记忆</button>' : '<a class="button" href="#/memory/new">新增第一条记忆</a>')}</section>
    ${data.total > 50 ? `<nav class="pagination" aria-label="记忆分页"><button data-action="previous" ${state.offset === 0 ? 'disabled' : ''}>上一页</button><span>第 ${Math.floor(state.offset / 50) + 1} 页</span><button data-action="next" ${state.offset + 50 >= data.total ? 'disabled' : ''}>下一页</button></nav>` : ''}`;
}

export async function memoryEditor(id) {
  const isNew = id === 'new';
  const m = isNew ? { text: '', category: '', tags: [], fact_time: '', project_id: '', sensitive: false } : await api(`/memories/${encodeURIComponent(id)}`);
  const response = await fetch('/assets/category-catalog.json');
  if (!response.ok) throw new Error('分类目录读取失败');
  const categories = (await response.json()).map(c => c.label);
  if (!isNew && !categories.includes(m.category)) categories.unshift(m.category);
  return `<div class="form-page"><a class="back" href="#/memories">← 返回我的记忆</a>${heading(isNew ? '新增记忆' : '编辑记忆', isNew ? '写下值得让助手长期了解的内容。' : '保留修改历史，让记忆跟上你的变化。')}
    <div class="detail-grid"><form id="memory-form" class="form-panel form-stack" data-id="${isNew ? '' : esc(id)}" data-revision="${m.revision || ''}"><label>记忆内容<textarea name="text" required maxlength="12000" placeholder="例如：学习新技术时，我更喜欢结合具体工程案例。">${esc(m.text)}</textarea></label><div class="form-grid"><label>范围<select name="project_id">${projectOptions(m.project_id)}</select></label><label>分类<select name="category">${categories.map(c => `<option ${m.category === c ? 'selected' : ''}>${esc(c)}</option>`).join('')}</select></label></div><label>标签<input name="tags" value="${esc(m.tags.join('，'))}" placeholder="用逗号分隔，例如：学习、表达方式"></label><label>事实时间 <small>可填“2025 年”“去年夏天”，不确定可以留空。</small><input name="fact_time" value="${esc(m.fact_time)}" maxlength="120" placeholder="这件事何时发生或生效"></label><label class="check-label"><input type="checkbox" name="sensitive" ${m.sensitive ? 'checked' : ''}><span>标记为敏感记忆<small>仅向获得敏感记忆权限的 Agent 提供。</small></span></label>${!isNew && m.state === 'uncertain' ? '<p class="help">这条记忆暂不确定，不会提供给助手。后续证据可自动澄清，你也可以直接编辑。</p>' : ''}${!isNew ? '<label>修改原因<input name="reason" maxlength="240" value="用户编辑"></label>' : ''}<p class="error" role="alert" id="form-error"></p><div class="form-actions"><button type="submit" class="primary">保存记忆</button><a class="button quiet" href="#/memories">取消</a>${!isNew ? `<button type="button" class="danger" data-action="delete-memory" data-id="${esc(id)}">删除</button>` : ''}</div></form><aside class="history"><h3>${isNew ? '清楚地表达，更容易被正确使用' : '版本历史'}</h3>${isNew ? '<p class="help">每条记忆只表达一件事。只适用于某个项目的约定，请选择对应项目。</p>' : m.versions.map(v => `<details class="history-item" ${v.revision === m.revision ? 'open' : ''}><summary>v${v.revision} · ${esc(dispositions[v.disposition] || '历史内容')}<small>${date(v.recorded_at)}</small></summary><p>${esc(v.content.text)}</p><small>${esc(v.reason)} · ${esc(operationNames[v.operation] || '用户编辑')}</small><small>${esc(v.content.fact_time || '事实时间未知')} · ${esc(projectName(v.content.project_id))}</small></details>`).join('')}${!isNew && m.sources?.length ? `<h3 class="sources-heading">信息来源</h3>${m.sources.map(source => `<details class="history-item"><summary>v${source.revision} · ${esc({user:'用户原话',assistant:'助手消息',tool:'工具观察'}[source.role] || '对话')}<small>${date(source.recorded_at)}</small></summary><p>${esc(source.quote)}</p></details>`).join('')}` : ''}</aside></div></div>`;
}

export function processingLabel(message) {
  if (message.processing_state === 'skipped' && message.processing_error === 'legacy_archive') return '原有归档';
  if (message.processing_state === 'done') {
    try { if (JSON.parse(message.processing_result)?.source_truncated) return '已整理部分原文'; } catch {}
  }
  return {pending:'等待整理',running:'整理中',done:'已整理',failed:'稍后重试',skipped:'未完成整理',cancelled:'已停止整理'}[message.processing_state] || '等待整理';
}

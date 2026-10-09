// Web UI: conversation and panel preview, updated by server events (SSE).
import { countLineChart, esc, headlineHtml, plainTable, renderPanel, thumbChart } from './charts.js';
import { LANG, tr } from './i18n.js';

const $ = (id) => document.getElementById(id);
const GREETING = tr('What panel do you need? Ask it as a question; if a definition is unclear, I will ask first.', '어떤 패널이 필요하세요? 질문으로 써 주시면 정의가 애매한 부분은 먼저 여쭤볼게요.');
const EMPTY_PREVIEW = () => `<div class="empty"><h2>${tr('Your panel appears here', '요청하면 여기에 패널이 그려집니다')}</h2><p>${tr('The engine checks the SQL the agent writes and runs it on both the real and pseudonymized copies before it shows up here. If a check fails, the previous preview stays.', '에이전트가 만든 SQL은 엔진이 검사하고 원본·가명 사본에서 모두 실행한 뒤에만 이 자리에 나타납니다. 검사에 실패하면 이전 미리보기를 그대로 둡니다.')}</p></div>`;

/** Static screen text in the configured language */
function applyStaticText() {
  const text = (id, s) => { const el = $(id); if (el) el.textContent = s; };
  const label = (sel, s) => { const el = document.querySelector(sel); if (el) el.textContent = s; };
  document.querySelector('.tabs')?.setAttribute('aria-label', tr('Views', '화면'));
  text('tab-work', tr('New panel', '새 패널'));
  text('tab-dash-label', tr('Dashboard', '대시보드'));
  text('tab-quality', tr('Data quality', '데이터 품질'));
  text('b-logout', tr('Sign out', '로그아웃'));
  text('login-title', tr('Sign in', '로그인'));
  label('label[for="login-user"]', tr('Username', '아이디'));
  label('label[for="login-pass"]', tr('Password', '비밀번호'));
  text('login-go', tr('Sign in', '로그인'));
  text('b-new', tr('Start over', '새로 시작'));
  label('label[for="prompt"]', tr('Request', '요청'));
  $('prompt').placeholder = tr('e.g. Weekly share of new members in the last 8 weeks who connected with another member', '예: 최근 8주 가입자 중 다른 회원과 연결된 비율을 주별로 보여줘');
  text('b-stop', tr('Stop', '중지'));
  text('send', tr('Send', '보내기'));
  for (const el of document.querySelectorAll('.placeholder p, #snapinfo .muted')) el.textContent = tr('Loading…', '불러오는 중…');
  const greet = document.querySelector('#log .msg.agent');
  if (greet) greet.textContent = GREETING;
  $('preview').innerHTML = EMPTY_PREVIEW();
}

const store = {
  get(k) { try { return localStorage.getItem(k); } catch { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch { /* works without storage */ } },
  remove(k) { try { localStorage.removeItem(k); } catch { /* ignore */ } },
};

async function api(path, body, method = 'POST') {
  const opt = body === undefined ? {} : { method, headers: { 'Content-Type': 'application/json', 'X-Growth-Lab': '1' }, body: method === 'DELETE' ? undefined : JSON.stringify(body) };
  const res = await fetch(path, opt);
  const data = await res.json().catch(() => ({}));
  if (res.status === 401 && path !== '/api/login' && path !== '/api/me') loggedOut();
  if (!res.ok) throw Object.assign(new Error(data.error ?? `HTTP ${res.status}`), { status: res.status });
  return data;
}

function div(cls, html) {
  const d = document.createElement('div');
  d.className = cls;
  d.innerHTML = html;
  return d;
}

// ── Tabs ────────────────────────────────────────────────
const tabs = { work: $('tab-work'), dash: $('tab-dash'), quality: $('tab-quality') };
const HASH = { work: '', dash: '#/dashboard', quality: '#/quality' };
let current = 'work';
const ROLE_RANK = { viewer: 0, editor: 1, admin: 2 };
let me = null;
const can = (role) => me && ROLE_RANK[me.role] >= ROLE_RANK[role];
const TAB_ROLE = { work: 'editor', dash: 'viewer', quality: 'admin' };

function show(name) {
  if (!me) return;
  if (!can(TAB_ROLE[name])) name = 'dash';
  current = name;
  $('view-login').hidden = true;
  for (const [k, b] of Object.entries(tabs)) b.setAttribute('aria-selected', String(k === name));
  for (const k of Object.keys(tabs)) $(`view-${k}`).hidden = k !== name;
  if (name === 'dash') void renderDash();
  if (name === 'quality') void renderQuality();
}
function go(hash) {
  if (location.hash === hash || (!location.hash && !hash)) route();
  else location.hash = hash;
}
/** #/dashboard, #/panels/<id>, #/quality; anything else is New panel */
function route() {
  if (!me) return;
  const h = location.hash;
  if (h.startsWith('#/panels/') || h === '#/dashboard') show('dash');
  else if (h === '#/quality') show('quality');
  else show('work');
}
for (const [k, b] of Object.entries(tabs)) b.addEventListener('click', () => go(HASH[k]));
window.addEventListener('hashchange', route);

// ── Top bar ─────────────────────────────────────────────
let appState = null;
async function loadState() {
  appState = await api('/api/state');
  const s = appState;
  const agentPill = s.agent ? ({ ok: 'ok', checking: 'neutral', failed: 'bad', disabled: 'bad' }[s.agent.state] ?? 'neutral') : null;
  $('snapinfo').innerHTML =
    (s.snapshot ? `<span>${tr('Snapshot', '스냅샷')} <span class="mono">${esc(s.snapshot.snapshot_id.slice(0, 15))}</span></span><span>${tr('As of', '기준')} ${esc(s.snapshot.as_of.slice(0, 16))}</span>` : `<span class="pill warn">${tr('No snapshot', '스냅샷 없음')}</span>`) +
    (s.agent ? `<span class="pill ${agentPill}"><span class="dot"></span>${esc(s.agent.message)}</span>` : '') +
    (s.data_mode === 'schema_only' ? `<span class="pill neutral">${tr('Result values not sent', '결과 값 비전송 모드')}</span>` : '');
  $('dash-count').textContent = s.dashboard_count ? String(s.dashboard_count) : '';
  if (s.agent?.state === 'checking') setTimeout(() => { if (me) void loadState(); }, 1500);
  return s;
}

/** Metric label: dictionary name or "not in dictionary" */
function metricHtml(metric) {
  if (metric === null || metric === undefined) return `<span class="pill warn">${tr('Not in dictionary', '사전 밖')}</span>`;
  const m = (appState?.metrics ?? []).find((x) => x.id === metric);
  return `<span class="pill neutral">${tr('Metric', '지표')}: ${esc(m ? m.name : metric)}</span>`;
}
const tablesText = (t) => (Array.isArray(t) ? (t.length ? t.join(', ') : tr('none', '없음')) : tr('not recorded', '기록 없음'));

// ── Conversation ────────────────────────────────────────
const log = $('log');
const ui = { conv: null, es: null, requestId: null, running: false, runningNode: null, lastInput: null, preview: null, sub: 'chart', chartState: {}, saveForm: null };

function add(node) {
  log.appendChild(node);
  log.scrollTop = log.scrollHeight;
  return node;
}
function step(cls, icon, html) {
  return add(div(`step ${cls}`, `<span class="ic">${icon}</span><div>${html}</div>`));
}
function setRunning(text) {
  if (!ui.runningNode) ui.runningNode = add(div('step run', '<span class="ic"><span class="spin"></span></span><div></div>'));
  ui.runningNode.lastChild.textContent = text;
  log.scrollTop = log.scrollHeight;
}
function stopRunning() {
  ui.runningNode?.remove();
  ui.runningNode = null;
}
function setBusy(b) {
  ui.running = b;
  $('b-stop').hidden = !b;
  const save = $('b-save');
  if (save) save.disabled = b;
}

function setSuggest(list) {
  const box = $('suggest');
  box.innerHTML = '';
  for (const t of list) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'chipbtn';
    b.textContent = t;
    b.addEventListener('click', () => submit(t));
    box.appendChild(b);
  }
}

async function submit(text) {
  if (!ui.conv) return;
  $('prompt').value = '';
  setSuggest([]);
  ui.lastInput = text;
  try {
    await api(`/api/conversations/${ui.conv}/messages`, { text });
  } catch (e) {
    step('fail', '!', `<span class="msgline">${tr('Could not send', '보내지 못했어요')}: ${esc(e.message)}</span>`);
  }
}

function questionCard(ev) {
  const qs = ev.data.questions;
  const picked = {};
  const c = div('card qcard', `<div class="qh">${tr('A few definitions to settle first. Unanswered items use the default.', '정의를 몇 가지만 정할게요. 답하지 않은 항목은 기본값으로 진행합니다.')}</div>`);
  for (const q of qs) {
    const box = div('q', `<span class="qt">${esc(q.text)}</span>`);
    const opts = div('opts', '');
    for (const o of q.options) {
      const b = document.createElement('button');
      b.type = 'button';
      b.className = 'opt';
      b.setAttribute('aria-pressed', 'false');
      b.innerHTML = esc(o.label) + (o.is_default ? `<span class="def">${tr('default', '기본')}</span>` : '');
      b.addEventListener('click', () => {
        opts.querySelectorAll('.opt').forEach((x) => x.setAttribute('aria-pressed', 'false'));
        b.setAttribute('aria-pressed', 'true');
        picked[q.id] = o.label;
        if (free) free.value = '';
      });
      opts.appendChild(b);
    }
    box.appendChild(opts);
    let free = null;
    if (q.allow_free_text) {
      free = document.createElement('input');
      free.className = 'free';
      free.maxLength = 300;
      free.placeholder = tr('Type your own (optional)', '직접 입력 (선택)');
      free.addEventListener('input', () => {
        if (free.value.trim()) {
          opts.querySelectorAll('.opt').forEach((x) => x.setAttribute('aria-pressed', 'false'));
          picked[q.id] = free.value.trim();
        } else delete picked[q.id];
      });
      box.appendChild(free);
    }
    c.appendChild(box);
  }
  const act = div('qactions', `<button type="button" class="btn" data-a="def">${tr('Use defaults', '기본값으로 진행')}</button><button type="button" class="btn primary" data-a="go">${tr('Send answers', '답변 보내기')}</button>`);
  c.appendChild(act);
  add(c);
  const send = async (useDefaults) => {
    c.querySelectorAll('button, input').forEach((x) => { x.disabled = true; });
    try {
      await api(`/api/conversations/${ui.conv}/answers`, { request_id: ev.request_id, turn_no: ev.turn_no, answers: useDefaults ? {} : picked });
    } catch (e) {
      step('fail', '!', `<span class="msgline">${tr('Could not send the answers', '답을 보내지 못했어요')}: ${esc(e.message)}</span>`);
    }
  };
  act.querySelector('[data-a="def"]').addEventListener('click', () => send(true));
  act.querySelector('[data-a="go"]').addEventListener('click', () => send(false));
}

function offdictCard(ev) {
  const d = ev.data;
  const s = d.spec;
  const c = div('card qcard offdict',
    `<div class="qh"><b>${tr('This panel uses a definition that is not in the metric dictionary.', '지표 사전에 없는 정의로 만든 패널이에요.')}</b> ${tr('Check the definition and tables, then choose.', '정의와 참조한 표를 확인한 뒤 고르세요.')}</div>` +
    `<div><b>${esc(s.title)}</b><div class="muted note-sm">${esc(s.question)}</div></div>` +
    `<dl class="def">${s.definition.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('')}<dt>${tr('Tables used', '참조한 표')}</dt><dd class="mono">${esc(tablesText(d.tables))}</dd></dl>` +
    `<div class="qactions"><button type="button" class="btn" data-a="no">${tr('Rebuild with a dictionary metric', '사전 지표로 다시 만들기')}</button><button type="button" class="btn primary" data-a="yes">${tr('View as an off-dictionary panel', '사전 밖 패널로 보기')}</button></div>`);
  add(c);
  const send = async (approve) => {
    c.querySelectorAll('button').forEach((x) => { x.disabled = true; });
    try {
      await api(`/api/conversations/${ui.conv}/offdict`, { request_id: ev.request_id, turn_no: ev.turn_no, approve });
    } catch (e) {
      step('fail', '!', `<span class="msgline">${tr('Could not send', '보내지 못했어요')}: ${esc(e.message)}</span>`);
      c.querySelectorAll('button').forEach((x) => { x.disabled = false; });
    }
  };
  c.querySelector('[data-a="yes"]').addEventListener('click', () => send(true));
  c.querySelector('[data-a="no"]').addEventListener('click', () => send(false));
}

function failedStep(ev) {
  const reasons = {
    turn_limit: tr('call limit', '호출 한도'), probe_limit: tr('probe limit', '탐색 한도'), fix_limit: tr('fix limit', '수정 한도'), request_budget: tr('request limit', '요청 한도'), call_budget: tr('call limit', '호출 한도'),
    isolation: tr('isolation check failed', '격리 점검 실패'), timeout: tr('timeout', '시간 초과'), process: tr('process error', '실행 오류'), rate_limit: tr('API limit', 'API 제한'), resume: tr('session resume failed', '세션 복구 실패'),
    schema: tr('response format error', '응답 형식 오류'), error: tr('error', '오류'), outbound_blocked: tr('blocked before sending', '전송 차단'), server_restart: tr('server restart', '서버 재시작'), unavailable: tr('unavailable', '사용할 수 없음'),
  };
  const node = step('fail', '!', `<b>${tr('Request failed', '요청 실패')}</b> <span class="muted">(${esc(reasons[ev.data.reason] ?? ev.data.reason)})</span><div class="msgline">${esc(ev.data.message)}</div><div class="retry"><button type="button" class="btn sm" data-a="retry">${tr('Try again', '다시 시도')}</button><button type="button" class="btn sm" data-a="new">${tr('New conversation', '새 대화로')}</button></div>`);
  node.querySelector('[data-a="retry"]').addEventListener('click', () => ui.lastInput && submit(ui.lastInput));
  node.querySelector('[data-a="new"]').addEventListener('click', newConversation);
}

/** Apply events of the current request only */
const PER_REQUEST = new Set(['request_started', 'step', 'plan', 'question', 'answers', 'offdict', 'offdict_answer', 'preview', 'refused', 'done', 'failed']);

function onEvent(ev) {
  const d = ev.data;
  if (ev.conversation_id && ev.conversation_id !== ui.conv) return;
  if (PER_REQUEST.has(ev.type) && ev.request_id !== ui.requestId) return;
  switch (ev.type) {
    case 'user_input':
      ui.requestId = ev.request_id;
      ui.lastInput = d.text;
      add(div('msg user', esc(d.text)));
      setBusy(true);
      setRunning(tr('Starting the request', '요청을 시작하는 중'));
      break;
    case 'queued':
      setRunning(tr('Stopping the previous request', '이전 요청을 멈추는 중'));
      break;
    case 'request_started':
      break;
    case 'step':
      if (d.kind === 'query_done') { stopRunning(); step('ok', '✓', `<b>${tr('Probe query', '탐색 쿼리')}</b> ${esc(d.text)}${d.ms !== null ? ` · ${(d.ms / 1000).toFixed(2)}${tr('s', '초')}` : ''} <div class="muted note-sm">${tr('The agent only sees results from the pseudonymized copy with replaced IDs', '에이전트에게는 ID를 바꾼 가명 사본의 결과만 전달됨')}</div>`); }
      else if (d.kind === 'check_failed') { stopRunning(); step('fail', '!', `<b>${tr('Panel check failed', '패널 검사 실패')}</b> ${esc(d.text)}`); }
      else if (d.kind === 'retry') { stopRunning(); step('run', '↻', esc(d.text)); }
      else if (d.kind === 'kept') { stopRunning(); step('ok', '✓', esc(d.text)); }
      else setRunning(d.text);
      break;
    case 'plan':
      stopRunning();
      add(div('plan', `<div class="label">${tr('Plan', '계획')}</div>${esc(d.text)}`));
      break;
    case 'question':
      stopRunning();
      setBusy(false);
      questionCard(ev);
      break;
    case 'offdict':
      stopRunning();
      setBusy(false);
      offdictCard(ev);
      break;
    case 'offdict_answer':
      add(div('msg user', d.approve ? tr('View as an off-dictionary panel', '사전 밖 패널로 보기') : tr('Rebuild with a dictionary metric', '사전 지표로 다시 만들기')));
      setBusy(true);
      setRunning(d.approve ? tr('Showing the preview', '미리보기를 띄우는 중') : tr('Rebuilding with a dictionary metric', '사전 지표로 다시 만드는 중'));
      break;
    case 'answers':
      add(div('msg user', d.answers.map((a) => esc(a.answer) + (a.defaulted ? ` <span class="soft">(${tr('default', '기본값')})</span>` : '')).join(' · ')));
      setBusy(true);
      setRunning(tr('Applying the answers', '답을 반영하는 중'));
      break;
    case 'preview':
      stopRunning();
      step('ok', '✓', `<b>${tr('Panel ready', '패널 완성')}</b> ${tr('checks and runs passed', '검사·실행 통과')} · ${d.preview.rows.length}${tr(' rows', '행')} · ${tr('real and pseudonymized results match', '원본/가명 결과 일치')}`);
      ui.preview = d.preview;
      ui.chartState = {};
      ui.saveForm = null;
      renderPreview();
      add(div('msg agent', tr('It is in the preview. Tell me if anything should change.', '미리보기에 띄웠어요. 고칠 점이 있으면 이어서 말씀해 주세요.')));
      setSuggest([tr('Make it a bar chart', '막대로 바꿔줘'), tr('Show only the last 4 weeks', '최근 4주만 보여줘')]);
      break;
    case 'refused':
      stopRunning();
      add(div('refused', `<b>${tr('This request cannot be done.', '만들 수 없는 요청이에요.')}</b> ${esc(d.reason)}${d.alternatives.length ? `<div class="muted mt4">${tr('You could ask this instead:', '대신 이렇게 물어보실 수 있어요:')}</div>` : ''}`));
      setSuggest(d.alternatives);
      break;
    case 'done':
      stopRunning();
      setBusy(false);
      renderPreview();
      break;
    case 'failed':
      stopRunning();
      setBusy(false);
      failedStep(ev);
      break;
    case 'cancelled':
      if (ev.request_id === ui.requestId) { stopRunning(); setBusy(false); }
      step('fail', '■', esc(d.message ?? tr('Request stopped', '요청을 멈췄어요')));
      break;
  }
}

function connect(since = 0) {
  ui.es?.close();
  const conv = ui.conv;
  const es = new EventSource(`/api/conversations/${ui.conv}/events?since=${since}`);
  es.onerror = checkSession;
  const types = ['user_input', 'queued', 'request_started', 'step', 'plan', 'question', 'answers', 'offdict', 'offdict_answer', 'preview', 'refused', 'done', 'failed', 'cancelled'];
  for (const t of types) es.addEventListener(t, (m) => onEvent(JSON.parse(m.data)));
  // Outside the server buffer: restore from the current state, then continue
  es.addEventListener('resync', () => {
    es.close();
    if (ui.conv !== conv || ui.es !== es) return;
    restore()
      .then((last) => { if (ui.conv === conv && ui.es === es) connect(last); })
      .catch(() => setTimeout(() => { if (ui.conv === conv && ui.es === es) connect(0); }, 2000));
  });
  ui.es = es;
}

function resetLog() {
  log.innerHTML = `<div class="msg agent">${esc(GREETING)}</div>`;
  ui.runningNode = null;
  setBusy(false);
}

/** Restore preview and questions from the server state; returns the last event number */
async function restore() {
  const conv = ui.conv;
  const s = await api(`/api/conversations/${conv}`);
  if (ui.conv !== conv) return s.last_event_id;
  resetLog();
  ui.requestId = s.request?.request_id ?? null;
  if (s.preview) { ui.preview = s.preview; renderPreview(); }
  if (s.question) questionCard({ request_id: s.question.request_id, turn_no: s.question.turn_no, data: { questions: s.question.questions } });
  if (s.failed) failedStep({ data: s.failed });
  if (s.offdict) offdictCard({ request_id: s.offdict.request_id, turn_no: s.offdict.turn_no, data: s.offdict });
  if (s.request && !['done', 'failed', 'cancelled', 'waiting_user'].includes(s.request.state)) { setBusy(true); setRunning(tr('In progress', '진행 중')); }
  return s.last_event_id;
}

async function newConversation() {
  const r = await api('/api/conversations', {});
  ui.conv = r.conversation_id;
  store.set('gl.conversation', ui.conv);
  ui.preview = null;
  ui.saveForm = null;
  resetLog();
  renderPreview();
  setSuggest(appState?.suggestions ?? []);
  $('conv-label').textContent = '';
  connect();
}

async function openConversation(id, regenerated = false) {
  let s;
  try {
    s = await api(`/api/conversations/${id}`);
  } catch {
    return newConversation();
  }
  ui.conv = id;
  ui.saveForm = null;
  ui.preview = s.preview ?? null;
  renderPreview();
  store.set('gl.conversation', id);
  $('conv-label').textContent = regenerated ? tr('Rebuilding a saved panel', '저장한 패널을 다시 만듭니다') : tr('Continuing the previous conversation', '이전 대화를 이어갑니다');
  if (s.last_event_id > 0) {
    resetLog();
    connect(0);
  } else {
    connect(await restore());
  }
}

// ── Preview ─────────────────────────────────────────────
function renderPreview() {
  const pv = $('preview');
  const p = ui.preview;
  if (!p) {
    pv.innerHTML = EMPTY_PREVIEW();
    return;
  }
  const s = p.spec;
  const defaulted = s.answers.filter((a) => a.defaulted).length;
  pv.innerHTML =
    `<div class="ph"><div><h2>${esc(s.title)}</h2><div class="q2">${esc(s.question)}</div><div class="metricline">${metricHtml(s.metric)}</div></div>` +
    `<div class="actions"><button class="btn primary" type="button" id="b-save"${ui.running || ui.saveForm ? ' disabled' : ''}>${tr('Save this panel', '현재 패널 저장')}</button></div></div>` +
    '<div id="saveslot"></div>' +
    headlineHtml(p.headline) +
    (p.caveats.length ? `<div class="caveats"><b>${tr('Before reading', '읽기 전에')}</b><ul>${p.caveats.map((c) => `<li>${esc(c)}</li>`).join('')}</ul></div>` : '') +
    `<div class="subtabs" role="tablist"><button role="tab" data-s="chart">${tr('Chart', '차트')}</button><button role="tab" data-s="def">${tr('Definition', '정의')}${defaulted ? ` <span class="tag defv">${tr('defaults', '기본값')} ${defaulted}</span>` : ''}</button><button role="tab" data-s="sql">SQL</button></div>` +
    '<div id="subview"></div>' +
    `<div class="runinfo"><span>${tr('Pattern', '패턴')} ${esc(p.display.type)}</span><span>${tr('Snapshot', '스냅샷')} <span class="mono">${esc(p.snapshot_id.slice(0, 15))}</span></span><span>${tr('As of', '기준')} ${esc(p.as_of.slice(0, 16))}</span><span>${p.rows.length}${tr(' rows', '행')}</span></div>`;
  $('b-save').addEventListener('click', openSaveForm);
  renderSaveForm();
  pv.querySelectorAll('.subtabs button').forEach((b) => {
    b.setAttribute('aria-selected', String(b.dataset.s === ui.sub));
    b.addEventListener('click', () => { ui.sub = b.dataset.s; renderPreview(); });
  });
  const sv = $('subview');
  if (ui.sub === 'chart') {
    const r = renderPanel({ display: p.display, columns: p.columns, rows: p.rows }, s.title, ui.chartState);
    const controls = r.controls
      ? `<div class="chart-controls"><label>${esc(r.controls.label)} <select id="chart-sel">${r.controls.options.map((o) => `<option${o === r.controls.value ? ' selected' : ''}>${esc(o)}</option>`).join('')}</select></label></div>`
      : '';
    sv.innerHTML = controls + r.html;
    $('chart-sel')?.addEventListener('change', (e) => { ui.chartState[r.controls.name] = e.target.value; renderPreview(); });
  } else if (ui.sub === 'def') {
    sv.innerHTML = `<dl class="def">${s.definition.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('')}` +
      s.answers.map((a) => `<dt>${esc(a.question)}</dt><dd>${esc(a.answer)}${a.defaulted ? ` <span class="tag defv">${tr('default used', '기본값 적용')}</span>` : ''}</dd>`).join('') +
      `<dt>${tr('Rates', '비율 계산')}</dt><dd>${tr('Computed by the engine as numerator / denominator', '엔진이 분자 / 분모로 계산')}</dd><dt>${tr('Tables used', '참조한 표')}</dt><dd class="mono">${esc(tablesText(p.tables))}</dd></dl>` +
      (p.plan ? `<p class="muted note-plan">${tr('Agent plan', '에이전트 계획')}: ${esc(p.plan)}</p>` : '');
  } else {
    sv.innerHTML = `<pre class="sql">${esc(s.sql)}</pre>`;
  }
}


// ── Save form ───────────────────────────────────────────
const SUMMARY_NOTE = {
  drafting: tr('The agent is writing a description from the result', '에이전트가 결과를 보고 설명을 쓰는 중이에요'),
  ok: tr('The engine rechecked every number in the description against the result. You can edit it', '설명 속 숫자는 엔진이 결과에서 다시 계산해 확인했어요. 고쳐 써도 됩니다'),
  failed: tr('Could not write a description automatically. You can write one yourself', '자동 설명을 만들지 못했어요. 직접 써도 됩니다'),
  disabled: tr('This workspace does not send result values, so there is no automatic description', '이 워크스페이스는 결과 값을 보내지 않는 모드라 자동 설명을 만들지 않아요'),
};

async function openSaveForm() {
  const p = ui.preview;
  if (!p || ui.saveForm) return;
  const f = { hash: p.preview_hash, requestId: p.request_id, title: p.spec.title, desc: p.spec.question.slice(0, 300), summary: '', summaryTouched: false, status: 'drafting', note: SUMMARY_NOTE.drafting, replaces: null, saving: false };
  ui.saveForm = f;
  renderPreview();
  try {
    const r = await api(`/api/conversations/${ui.conv}/save-draft`, { request_id: f.requestId, preview_hash: f.hash });
    if (ui.saveForm !== f) return;
    f.status = r.summary_status;
    f.note = SUMMARY_NOTE[r.summary_status] ?? '';
    if (!f.summaryTouched) f.summary = r.summary;
  } catch (e) {
    if (ui.saveForm !== f) return;
    f.status = 'failed';
    f.note = `${SUMMARY_NOTE.failed} (${e.message})`;
  }
  renderSaveForm();
}

function renderSaveForm() {
  const slot = $('saveslot');
  const f = ui.saveForm;
  if (!slot) return;
  if (!f) { slot.innerHTML = ''; return; }
  if (f.replaces) {
    slot.innerHTML = `<div class="saveform"><b>${tr('Saved to the dashboard.', '대시보드에 저장했어요.')}</b><div>${tr('This panel rebuilds an existing one. Delete the old panel?', '이 패널은 기존 패널을 다시 만든 것이에요. 기존 패널을 지울까요?')}</div><div class="qactions"><button type="button" class="btn" data-a="keep">${tr('Keep it', '그대로 두기')}</button><button type="button" class="btn danger" data-a="del">${tr('Delete old panel', '기존 패널 삭제')}</button></div></div>`;
    slot.querySelector('[data-a="keep"]').addEventListener('click', closeSaveForm);
    slot.querySelector('[data-a="del"]').addEventListener('click', async () => {
      try { await api(`/api/panels/${f.replaces}`, {}, 'DELETE'); toast(tr('Old panel deleted', '기존 패널을 지웠어요')); } catch (e) { toast(`${tr('Could not delete', '지우지 못했어요')}: ${e.message}`); }
      closeSaveForm();
    });
    return;
  }
  const drafting = f.status === 'drafting';
  slot.innerHTML =
    '<form class="saveform" id="sf">' +
    `<div class="sf-row"><label for="sf-title">${tr('Title', '제목')}</label><input id="sf-title" maxlength="60" required value="${esc(f.title)}"></div>` +
    `<div class="sf-row"><label for="sf-desc">${tr('Short description', '짧은 설명')}</label><textarea id="sf-desc" rows="2" maxlength="300">${esc(f.desc)}</textarea></div>` +
    `<div class="sf-row"><label for="sf-sum">${tr('Description', '긴 설명')}</label><div><textarea id="sf-sum" rows="5" maxlength="2000"${drafting ? ` disabled placeholder="${tr('Writing the description…', '설명 작성 중…')}"` : ''}>${esc(f.summary)}</textarea><div class="muted note-sm">${drafting ? '<span class="spin inline"></span> ' : ''}${esc(f.note)}</div></div></div>` +
    `<div class="qactions"><button type="button" class="btn" id="sf-cancel">${tr('Cancel', '취소')}</button><button type="submit" class="btn primary" id="sf-save"${f.saving ? ' disabled' : ''}>${tr('Save', '저장')}</button></div>` +
    '</form>';
  $('sf-title').addEventListener('input', (e) => { f.title = e.target.value; });
  $('sf-desc').addEventListener('input', (e) => { f.desc = e.target.value; });
  $('sf-sum').addEventListener('input', (e) => { f.summary = e.target.value; f.summaryTouched = true; });
  $('sf-cancel').addEventListener('click', closeSaveForm);
  $('sf').addEventListener('submit', (e) => { e.preventDefault(); void submitSave(f); });
}

function closeSaveForm() {
  ui.saveForm = null;
  renderPreview();
}

async function submitSave(f) {
  if (!f.title.trim()) return;
  f.saving = true;
  renderSaveForm();
  try {
    const r = await api('/api/panels', { conversation_id: ui.conv, request_id: f.requestId, preview_hash: f.hash, title: f.title.trim(), description: f.desc.trim(), summary: f.status === 'drafting' ? '' : f.summary.trim() });
    if (ui.saveForm !== f) return;
    toast(r.created ? tr('Saved to the dashboard', '대시보드에 저장했어요') : tr('This panel is already saved', '이미 저장된 패널이에요'));
    void loadState();
    if (r.replaces) { f.replaces = r.replaces; renderSaveForm(); } else closeSaveForm();
  } catch (e) {
    f.saving = false;
    f.note = `${tr('Could not save', '저장하지 못했어요')}: ${e.message}`;
    renderSaveForm();
  }
}

let toastTimer = null;
function toast(text) {
  let t = document.querySelector('.toast');
  if (!t) { t = document.createElement('div'); t.className = 'toast'; t.setAttribute('role', 'status'); document.body.appendChild(t); }
  t.textContent = text;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.remove(), 2600);
}


// ── Dashboard and detail ────────────────────────────────
const STATUS_BADGE = {
  review: ['warn', tr('Needs review', '재검토 필요')],
  recompute_failed: ['bad', tr('Recompute failed', '재계산 실패')],
  id_dependent: ['bad', tr('Depends on ID values', 'ID 값 의존')],
};
const JOB_BADGE = { queued: tr('Recompute queued', '재계산 대기'), running: tr('Recomputing', '재계산 중'), cancelling: tr('Stopping', '중지하는 중') };
const MODE_LABEL = { preview: tr('Computed when saved', '저장할 때 계산'), auto: tr('Recomputed automatically on a new snapshot', '새 스냅샷으로 자동 재계산'), manual_rule: tr('Recomputed manually after a rule change', '규칙 변경 후 수동 재계산') };
const RULE_LABEL = { schema_version: tr('derived rules', '파생 규칙'), policy_version: tr('run policy', '실행 정책'), docs_version: tr('guide and example panels', '설명서·예시 패널'), prompt_version: tr('agent instructions', '에이전트 지침'), pattern_contract_version: tr('result contract', '결과 계약') };
const dash = { panels: [], detailState: {}, confirmDelete: false, busy: null };

const when = (iso) => (iso ? new Date(iso).toLocaleString(LANG === 'ko' ? 'ko-KR' : 'en-US', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '–');
const viewOf = (p) => ({ display: p.display, columns: p.last_result.columns, rows: p.last_result.rows });
const jobActive = (j) => j === 'queued' || j === 'running' || j === 'cancelling';

function badges(p) {
  const out = [];
  const b = STATUS_BADGE[p.status];
  if (b) out.push(`<span class="pill ${b[0]}">${b[1]}</span>`);
  if (JOB_BADGE[p.job_status]) out.push(`<span class="pill neutral"><span class="spin inline"></span>${JOB_BADGE[p.job_status]}</span>`);
  return out.join('');
}

async function loadPanels() {
  dash.panels = (await api('/api/panels')).panels;
  $('dash-count').textContent = dash.panels.length ? String(dash.panels.length) : '';
}

async function renderDash() {
  const page = $('view-dash').querySelector('.page');
  try {
    await loadPanels();
  } catch (e) {
    page.innerHTML = `<div class="placeholder"><h2>${tr('Dashboard', '대시보드')}</h2><p>${tr('Could not load', '불러오지 못했어요')}: ${esc(e.message)}</p></div>`;
    return;
  }
  if (current !== 'dash') return;
  const m = /^#\/panels\/([a-z0-9]{12})$/.exec(location.hash);
  if (m) return renderDetail(page, m[1]);
  if (!dash.panels.length) {
    page.innerHTML = `<div class="placeholder"><h2>${tr('No saved panels yet', '아직 저장한 패널이 없어요')}</h2><p>${tr('Build a panel in the New panel tab and press [Save this panel] to collect it here. Saved panels are recomputed when a new snapshot arrives.', '새 패널 탭에서 패널을 만들고 [현재 패널 저장]을 누르면 여기에 모입니다. 새 스냅샷이 들어오면 저장한 패널을 다시 계산해요.')}</p></div>`;
    return;
  }
  page.innerHTML = `<div class="page-head"><h2>${tr('Dashboard', '대시보드')}</h2><span class="muted note-sm">${tr(`${dash.panels.length} panels`, `패널 ${dash.panels.length}개`)} · ${tr('recomputed automatically on a new snapshot', '새 스냅샷이 들어오면 자동으로 다시 계산')}</span></div><div class="dgrid">${dash.panels.map(cardHtml).join('')}</div>`;
  page.querySelectorAll('.dcard').forEach((c) => {
    const open = () => go(`#/panels/${c.dataset.id}`);
    c.addEventListener('click', (e) => { if (!e.target.closest('button')) open(); });
    c.addEventListener('keydown', (e) => { if ((e.key === 'Enter' || e.key === ' ') && e.target === c) { e.preventDefault(); open(); } });
  });
  page.querySelectorAll('[data-stop]').forEach((b) => b.addEventListener('click', () => cancelJob(b.dataset.stop)));
}

function cardHtml(p) {
  const r = p.last_result;
  const faded = p.status !== 'ok' ? ' faded' : '';
  let thumb = null;
  try { thumb = thumbChart(viewOf(p), p.title); } catch { thumb = null; }
  const type = p.display.type;
  const body = r.headline ? headlineHtml(r.headline) : `<span class="muted note-sm">${type === 'cohort' ? tr('Cohort table', '코호트 표') : tr(`Table, ${r.rows.length} rows`, `표 ${r.rows.length}행`)}</span>`;
  return `<article class="dcard${p.status === 'review' ? ' review' : ''}" tabindex="0" data-id="${esc(p.id)}" aria-label="${esc(tr(`${p.title} details`, `${p.title} 상세 보기`))}">` +
    `<div class="dtop"><h3>${esc(p.title)}</h3><div class="badges">${badges(p)}</div></div>` +
    `<div class="metricline">${metricHtml(p.metric)}</div>` +
    (p.description ? `<p class="ddesc">${esc(p.description)}</p>` : '') +
    `<div class="dbody">${body}</div>` +
    (thumb ? `<div class="thumb${faded}">${thumb}</div>` : '') +
    `<div class="dfoot">${tr('Last computed', '마지막 계산')} ${esc(when(r.computed_at))} · ${tr('as of', '기준')} ${esc(r.as_of.slice(0, 16))}${p.full && jobActive(p.job_status) && p.job_status !== 'cancelling' ? ` <button type="button" class="btn sm" data-stop="${esc(p.id)}">${tr('Stop', '중지')}</button>` : ''}</div>` +
    '</article>';
}

async function cancelJob(id) {
  try { await api(`/api/panels/${id}/recompute/cancel`, {}); } catch (e) { toast(`${tr('Could not stop', '중지하지 못했어요')}: ${e.message}`); }
}

function renderDetail(page, id) {
  const p = dash.panels.find((x) => x.id === id);
  if (!p) {
    page.innerHTML = `<button type="button" class="back" id="d-back">← ${tr('Dashboard', '대시보드')}</button><div class="placeholder"><h2>${tr('No such panel', '없는 패널이에요')}</h2><p>${tr('It may have been deleted.', '삭제되었을 수 있어요.')}</p></div>`;
    $('d-back').addEventListener('click', () => go('#/dashboard'));
    return;
  }
  const r = p.last_result;
  const full = p.full === true;
  const chart = renderPanel(viewOf(p), p.title, dash.detailState);
  const controls = chart.controls
    ? `<div class="chart-controls"><label>${esc(chart.controls.label)} <select id="d-sel">${chart.controls.options.map((o) => `<option${o === chart.controls.value ? ' selected' : ''}>${esc(o)}</option>`).join('')}</select></label></div>`
    : '';
  const canRule = full && (p.status === 'review' || p.status === 'recompute_failed') && !p.legacy;
  const running = jobActive(p.job_status);
  let note = '';
  if (p.status === 'review') { const rules = (p.changed_rules ?? []).map((k) => RULE_LABEL[k] ?? k).join(', ') || tr('version', '버전'); note = tr(`Not recomputed automatically because rules changed (${rules}). Below is the result under the previous rules.`, `규칙이 바뀌어(${rules}) 자동으로 다시 계산하지 않았어요. 아래는 이전 규칙 기준 결과예요.`); }
  if (p.status === 'recompute_failed') note = `${tr('Could not recompute with the new snapshot; showing the previous result.', '새 스냅샷으로 다시 계산하지 못해 이전 결과를 보여 줘요.')} ${p.last_error?.message ?? ''}`;
  if (p.legacy) note = tr('This panel was saved before the metric dictionary and cannot be recomputed. Regenerate it with the agent.', '지표 사전 이전에 저장한 패널이라 다시 계산할 수 없어요. 에이전트로 재생성해 주세요.');
  if (p.status === 'id_dependent') note = tr('The real and pseudonymized results differ, so this panel depends on ID values. Showing the previous result. Rebuild it with the agent.', '원본과 가명 사본의 결과가 달라 ID 값에 의존하는 패널로 판정했어요. 이전 결과를 보여 줘요. 에이전트로 다시 만들어 주세요.');
  const summaryOld = p.summary && p.summary_snapshot_id && p.summary_snapshot_id !== r.snapshot_id;
  const schemaOnly = appState?.data_mode === 'schema_only';
  const defs = p.definition.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${esc(v)}</dd>`).join('') +
    (p.answers ?? []).map((a) => `<dt>${esc(a.question)}</dt><dd>${esc(a.answer)}${a.defaulted ? ` <span class="tag defv">${tr('default', '기본값')}</span>` : ''}</dd>`).join('');
  page.innerHTML =
    `<button type="button" class="back" id="d-back">← ${tr('Dashboard', '대시보드')}</button>` +
    `<div class="dd-head"><div><h1>${esc(p.title)}</h1><div class="metricline">${metricHtml(p.metric)}</div>${p.description ? `<p class="dd-lead">${esc(p.description)}</p>` : ''}</div>` +
    `<div class="dd-acts">${badges(p)}${full && running && p.job_status !== 'cancelling' ? `<button type="button" class="btn sm" id="d-stop">${tr('Stop', '중지')}</button>` : ''}</div></div>` +
    (note ? `<div class="reviewnote"><span>${esc(note)}</span>${canRule ? `<button type="button" class="btn sm" id="d-rule"${running ? ' disabled' : ''}>${tr('Recompute with these rules', '이 규칙으로 다시 계산')}</button>` : ''}${full && p.status !== 'ok' ? `<button type="button" class="btn sm" id="d-regen">${tr('Regenerate with agent', '에이전트로 재생성')}</button>` : ''}</div>` : '') +
    '<div class="dd-grid"><div class="dd-main">' +
    headlineHtml(r.headline) +
    (r.caveats.length ? `<div class="caveats"><b>${tr('Before reading', '읽기 전에')}</b><ul>${r.caveats.map((c) => `<li>${esc(c)}</li>`).join('')}</ul></div>` : '') +
    `<div class="${p.status !== 'ok' ? 'faded-wrap' : ''}">${controls}${chart.html}</div>` +
    `<h2 class="sec">${tr('What this panel says', '이 패널이 말해 주는 것')}</h2>` +
    (p.summary ? `<p class="prose">${esc(p.summary)}</p><p class="muted note-sm">${tr('Written for snapshot', '작성 시점 스냅샷')} <span class="mono">${esc((p.summary_snapshot_id ?? '').slice(0, 15))}</span>${tr('', ' 기준')}${summaryOld ? tr(' · numbers may differ from the current result', ' · 지금 결과와 숫자가 다를 수 있어요') : ''}</p>` : `<p class="muted">${schemaOnly ? tr('This workspace does not send result values, so there is no automatic description.', '이 워크스페이스는 결과 값을 보내지 않는 모드라 자동 설명이 없어요.') : tr('No description.', '설명이 없어요.')}</p>`) +
    (schemaOnly || !full ? '' : `<div><button type="button" class="btn sm" id="d-resum"${dash.busy === 'resum' || running ? ' disabled' : ''}>${dash.busy === 'resum' ? tr('Writing…', '설명 쓰는 중…') : tr('Rewrite description', '설명 다시 쓰기')}</button></div>`) +
    (full ? `<details class="raw"><summary>${tr('Show SQL', 'SQL 보기')}</summary><pre class="sql">${esc(p.sql)}</pre></details>` : '') +
    '</div><aside class="dd-side">' +
    (full ? `<div class="side-block"><h3>${tr('Original request', '처음 요청')}</h3><p class="quote">${esc(p.prompt)}</p></div>` : '') +
    `<div class="side-block"><h3>${tr('Definitions', '정한 정의')}</h3><dl class="def">${defs}<dt>${tr('Rates', '비율 계산')}</dt><dd>${tr('Computed by the engine as numerator / denominator', '엔진이 분자 / 분모로 계산')}</dd></dl></div>` +
    `<div class="side-block"><h3>${tr('Computation', '계산 정보')}</h3><dl class="def"><dt>${tr('Last computed', '마지막 계산')}</dt><dd>${esc(when(r.computed_at))}</dd><dt>${tr('Update mode', '갱신 방식')}</dt><dd>${esc(MODE_LABEL[r.mode] ?? r.mode)}</dd><dt>${tr('Snapshot', '스냅샷')}</dt><dd><span class="mono">${esc(r.snapshot_id.slice(0, 15))}</span></dd><dt>${tr('Data as of', '데이터 기준')}</dt><dd>${esc(r.as_of.slice(0, 16))}</dd><dt>${tr('Pattern', '패턴')}</dt><dd>${esc(p.display.type)} · ${r.rows.length}${tr(' rows', '행')}</dd>${full ? `<dt>${tr('Tables used', '참조한 표')}</dt><dd class="mono">${esc(tablesText(p.tables))}</dd>` : ''}<dt>${tr('Saved', '저장')}</dt><dd>${esc(when(p.created_at))}${p.created_by ? ` · ${esc(p.created_by)}` : ''}</dd></dl></div>` +
    (!full ? '' : `<div class="side-block">${dash.confirmDelete ? `<div>${tr('Delete this panel? This cannot be undone.', '이 패널을 삭제할까요? 되돌릴 수 없어요.')}</div><div class="qactions"><button type="button" class="btn sm" id="d-del-no">${tr('Cancel', '취소')}</button><button type="button" class="btn sm danger" id="d-del-yes">${tr('Delete', '삭제')}</button></div>` : `<button type="button" class="btn sm" id="d-del"${running ? ' disabled' : ''}>${tr('Delete panel', '패널 삭제')}</button>`}</div>`) +
    '</aside></div>';

  $('d-back').addEventListener('click', () => go('#/dashboard'));
  $('d-sel')?.addEventListener('change', (e) => { dash.detailState[chart.controls.name] = e.target.value; renderDetail(page, id); });
  $('d-stop')?.addEventListener('click', () => cancelJob(id));
  $('d-rule')?.addEventListener('click', async () => {
    try { await api(`/api/panels/${id}/recompute`, {}); } catch (e) { toast(`${tr('Could not recompute', '다시 계산하지 못했어요')}: ${e.message}`); }
    void renderDash();
  });
  $('d-regen')?.addEventListener('click', async () => {
    try {
      const r2 = await api(`/api/panels/${id}/regenerate`, {});
      go('');
      await openConversation(r2.conversation_id, true);
    } catch (e) { toast(`${tr('Could not start regenerating', '재생성을 시작하지 못했어요')}: ${e.message}`); }
  });
  $('d-resum')?.addEventListener('click', async () => {
    dash.busy = 'resum';
    renderDetail(page, id);
    try {
      const r2 = await api(`/api/panels/${id}/resummarize`, {});
      toast(r2.summary_status === 'ok' ? tr('Description rewritten', '설명을 다시 썼어요') : `${tr('Could not write the description', '설명을 만들지 못했어요')}: ${r2.message}`);
    } catch (e) { toast(`${tr('Could not write the description', '설명을 만들지 못했어요')}: ${e.message}`); }
    dash.busy = null;
    void renderDash();
  });
  $('d-del')?.addEventListener('click', () => { dash.confirmDelete = true; renderDetail(page, id); });
  $('d-del-no')?.addEventListener('click', () => { dash.confirmDelete = false; renderDetail(page, id); });
  $('d-del-yes')?.addEventListener('click', async () => {
    dash.confirmDelete = false;
    try {
      await api(`/api/panels/${id}`, {}, 'DELETE');
      toast(tr('Panel deleted', '패널을 삭제했어요'));
      go('#/dashboard');
    } catch (e) { toast(`${tr('Could not delete', '삭제하지 못했어요')}: ${e.message}`); renderDetail(page, id); }
  });
}

// ── Data quality ────────────────────────────────────────
async function renderQuality() {
  const page = $('view-quality').querySelector('.page');
  let q;
  try {
    q = await api('/api/quality');
  } catch (e) {
    page.innerHTML = `<div class="placeholder"><h2>${tr('Data quality', '데이터 품질')}</h2><p>${tr('Could not load', '불러오지 못했어요')}: ${esc(e.message)}</p></div>`;
    return;
  }
  if (current !== 'quality') return;
  const status = q.status === 'idle' ? '' : `<span class="pill neutral"><span class="spin inline"></span>${q.status === 'running' ? tr('Checking', '검사 중') : tr('Check queued', '검사 대기')}</span>`;
  const head = `<div class="page-head"><h2>${tr('Data quality', '데이터 품질')}</h2><span class="muted note-sm">${status} ${q.snapshot_id ? `${tr('Snapshot', '스냅샷')} <span class="mono">${esc(q.snapshot_id.slice(0, 15))}</span> · ${tr('checked', '검사')} ${esc(when(q.computed_at))}` : ''}</span></div>`;
  if (!q.items.length) {
    page.innerHTML = head + `<div class="placeholder"><p>${q.error ? `${tr('Could not check', '검사하지 못했어요')}: ${esc(q.error)}` : q.status === 'idle' ? tr('No check results yet.', '아직 검사 결과가 없어요.') : tr('Results appear here when the check finishes.', '검사가 끝나면 여기에 나와요.')}</p></div>`;
    return;
  }
  page.innerHTML = head + (q.error ? `<div class="reviewnote">${esc(q.error)}</div>` : '') + q.items.map((it) => {
    const view = { columns: it.columns.map((name) => ({ name })), rows: it.rows };
    const body = it.error
      ? `<div class="reviewnote">${tr('Check failed', '검사 실패')}: ${esc(it.error)}</div>`
      : it.display === 'line' && it.rows.length
        ? `${countLineChart(it.columns, it.rows, it.title)}<details class="raw"><summary>${tr(`Show table (${it.rows.length} rows)`, `표 보기 (${it.rows.length}행)`)}</summary>${plainTable(view)}</details>`
        : plainTable(view);
    return `<section class="side-block qitem"><h3 class="qitem-h">${esc(it.title)} <span class="mono muted">${esc(it.id)}</span>${it.builtin ? `<span class="tag">${tr('built-in', '엔진 내장')}</span>` : ''}</h3>${body}</section>`;
  }).join('');
}

// ── Panel events (global) ───────────────────────────────
let refreshTimer = null;
function refreshSoon() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => {
    if (current === 'dash') void renderDash();
    else void loadPanels().catch(() => {});
  }, 250);
}
let panelEs = null;
function connectPanels() {
  panelEs?.close();
  const es = new EventSource('/api/panels/events');
  es.onerror = checkSession;
  panelEs = es;
  for (const t of ['panel_status_all', 'panel_status', 'panels_changed']) es.addEventListener(t, refreshSoon);
  es.addEventListener('snapshot_changed', () => { void loadState(); refreshSoon(); });
  es.addEventListener('quality_status', () => { if (current === 'quality') void renderQuality(); });
}

// ── Input ───────────────────────────────────────────────
$('composer').addEventListener('submit', (e) => {
  e.preventDefault();
  const v = $('prompt').value.trim();
  if (v) void submit(v);
});
$('prompt').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    $('composer').requestSubmit();
  }
});
$('b-stop').addEventListener('click', () => api(`/api/conversations/${ui.conv}/stop`, {}).catch(() => {}));
$('b-new').addEventListener('click', () => void newConversation());

try {
  matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => ui.preview && renderPreview());
} catch { /* ignore */ }

// ── Sign-in ─────────────────────────────────────────────
function showLogin() {
  for (const k of Object.keys(tabs)) $(`view-${k}`).hidden = true;
  $('view-login').hidden = false;
  $('userbar').hidden = true;
  $('snapinfo').innerHTML = '';
  document.querySelector('.tabs').hidden = true;
  $('login-user').focus();
}

/** When a stream drops, check whether the session is still alive */
let checking = false;
async function checkSession() {
  if (checking || !me) return;
  checking = true;
  try {
    const r = await fetch('/api/me');
    if (r.status === 401) loggedOut();
  } catch { /* EventSource reconnects after network errors */ } finally {
    checking = false;
  }
}

/** On sign-out: close streams, clear screen state, show the sign-in screen */
function loggedOut() {
  if (!me) return showLogin();
  me = null;
  ui.es?.close();
  ui.es = null;
  panelEs?.close();
  panelEs = null;
  ui.conv = null;
  ui.preview = null;
  ui.saveForm = null;
  ui.requestId = null;
  ui.lastInput = null;
  ui.chartState = {};
  ui.sub = 'chart';
  dash.panels = [];
  dash.detailState = {};
  dash.confirmDelete = false;
  dash.busy = null;
  appState = null;
  store.remove('gl.conversation');
  setSuggest([]);
  $('conv-label').textContent = '';
  $('dash-count').textContent = '';
  $('login-pass').value = '';
  resetLog();
  $('preview').innerHTML = '';
  for (const k of ['dash', 'quality']) $(`view-${k}`).querySelector('.page').innerHTML = '';
  showLogin();
}

async function startApp() {
  $('view-login').hidden = true;
  document.querySelector('.tabs').hidden = false;
  $('userbar').hidden = false;
  $('user-name').textContent = `${me.username} · ${{ viewer: tr('viewer', '보기 전용'), editor: tr('editor', '작성자'), admin: tr('admin', '관리자') }[me.role] ?? me.role}`;
  $('b-logout').hidden = me.auth === false;
  for (const [k, b] of Object.entries(tabs)) b.hidden = !can(TAB_ROLE[k]);
  await loadState();
  connectPanels();
  if (can('editor')) {
    // No conversation remembered in this browser (e.g. new sign-in): ask the server for my latest one
    const saved = store.get('gl.conversation');
    const latest = saved && /^[a-z0-9]{12}$/.test(saved) ? saved : (await api('/api/conversations').catch(() => ({ conversation_id: null }))).conversation_id;
    if (latest) await openConversation(latest);
    else await newConversation();
    if (!ui.preview && log.children.length <= 1) setSuggest(appState?.suggestions ?? []);
  }
  if (!can('editor') && !location.hash) location.hash = '#/dashboard';
  route();
}

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  $('login-err').textContent = '';
  $('login-go').disabled = true;
  try {
    me = await api('/api/login', { username: $('login-user').value.trim(), password: $('login-pass').value });
    $('login-pass').value = '';
    await startApp();
  } catch (err) {
    $('login-err').textContent = err.status === 429 ? tr('Please try again in a moment', '잠시 뒤 다시 시도해 주세요') : err.message;
  } finally {
    $('login-go').disabled = false;
  }
});
$('b-logout').addEventListener('click', async () => {
  try { await api('/api/logout', {}); } catch { /* already signed out */ }
  loggedOut();
});

// ── Start ───────────────────────────────────────────────
applyStaticText();
(async () => {
  try {
    me = await api('/api/me');
  } catch {
    return showLogin();
  }
  await startApp();
})();

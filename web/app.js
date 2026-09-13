'use strict';

// Стенд управления контекстом. Дорожки отличаются одним — тем, что видят
// вместо истории разговора, — поэтому разница в ответах и есть разница
// стратегий. Одиночный диалог устроен так же, но дорожка у него одна,
// зато её можно ветвить: поставить точку, развести от неё два
// продолжения и ходить между ними.
//
// Диалоги живут на сервере и на диске: здесь их список, открытый стенд
// целиком и ходы, которые идут прямо сейчас (журнал приходит потоком SSE).

const MODES = {
  full: {
    name: 'Вся история',
    rule: 'весь разговор уходит модели заново каждый ход',
    tone: 'var(--full)'
  },
  window: {
    name: 'Окно',
    rule: 'только последние сообщения, всё старше выброшено',
    tone: 'var(--window)'
  },
  facts: {
    name: 'Факты',
    rule: 'последние сообщения плюс карточка «ключ — значение»',
    tone: 'var(--facts)'
  }
};

const MODE_ORDER = ['full', 'window', 'facts'];

const state = {
  agents: [],
  strategy: null,       // настройки стратегии на сервере
  strategies: MODE_ORDER,
  serverStarted: null,
  list: [],             // сводки всех диалогов с сервера
  view: null,           // открытый стенд: { kind, id, title, lanes }
  live: {},             // turnId → { view, events }
  sources: {},          // turnId → EventSource
  draftAgent: null,
  draftKind: 'comparison',
  draftMode: 'facts',
  fileLane: 0,
  // Ходы, у которых ответ развёрнут целиком. Лента перерисовывается на
  // каждое событие идущего хода, и без этого развёрнутый ответ схлопывался
  // бы сам собой посреди чтения.
  expanded: new Set()
};

const el = {};
for (const id of [
  'model-name', 'strategy-note', 'history-dir', 'server-started',
  'new-button', 'conversations', 'conversations-empty',
  'stage-title', 'stage-note', 'file-button', 'delete-button',
  'switcher', 'switcher-buttons', 'switcher-rule',
  'branches', 'branch-list', 'checkpoint-list', 'mark-button', 'fork-button',
  'lane-heads', 'tape', 'tape-empty',
  'composer', 'text', 'send-button', 'error',
  'new-dialog', 'new-close', 'new-create', 'agent-choice', 'kind-choice',
  'strategy-choice', 'single-strategy',
  'file-dialog', 'file-close', 'file-tabs', 'file-path', 'file-body'
]) {
  el[id.replace(/-([a-z])/g, (_, c) => c.toUpperCase())] = document.getElementById(id);
}

const KIND_LABEL = {
  'agent.start': 'агент',
  'agent.done': 'готово',
  'agent.error': 'ошибка',
  'llm.request': 'модель ←',
  'llm.response': 'модель →',
  'tool.call': 'инструмент ←',
  'tool.result': 'инструмент →',
  'tool.error': 'инструмент ✕',
  'note': 'заметка',
  'prompt': 'промпт',
  'facts.update': 'факты',
  'branch': 'ветка'
};

async function init() {
  if (window.marked) {
    marked.use({
      gfm: true,
      breaks: true,
      renderer: {
        // Сырой HTML из ответа модели показываем как текст.
        html(token) {
          const raw = typeof token === 'string' ? token : (token.raw ?? token.text ?? '');
          return escapeHTML(raw);
        }
      }
    });
  }

  try {
    const data = await getJSON('/api/agents');
    state.agents = data.agents || [];
    state.strategy = data.strategy || null;
    state.strategies = data.strategies || MODE_ORDER;
    state.draftMode = (state.strategy && state.strategy.mode) || 'facts';
    state.serverStarted = data.serverStarted ? new Date(data.serverStarted) : null;
    el.historyDir.textContent = data.historyDir || '—';
    el.strategyNote.textContent = strategyNote(state.strategy);
    el.serverStarted.textContent = state.serverStarted ? formatTime(state.serverStarted) : '—';
  } catch (err) {
    showError('Не удалось получить настройки сервера: ' + err.message);
  }

  renderPicker();
  el.newButton.addEventListener('click', openPicker);
  el.newClose.addEventListener('click', () => el.newDialog.close());
  el.newCreate.addEventListener('click', () => {
    const agent = el.agentChoice.querySelector('input:checked');
    state.draftAgent = agent ? agent.value : (state.agents[0] && state.agents[0].key);
    const kind = el.kindChoice.querySelector('input:checked');
    state.draftKind = kind ? kind.value : 'comparison';
    const mode = el.strategyChoice.querySelector('input:checked');
    state.draftMode = mode ? mode.value : state.draftMode;
    el.newDialog.close();
    openBlank();
  });
  el.composer.addEventListener('submit', (e) => { e.preventDefault(); ask(); });
  el.text.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.ctrlKey) { e.preventDefault(); ask(); }
  });
  el.fileButton.addEventListener('click', showFiles);
  el.fileClose.addEventListener('click', () => el.fileDialog.close());
  el.deleteButton.addEventListener('click', dropCurrent);
  el.markButton.addEventListener('click', markCheckpoint);
  el.forkButton.addEventListener('click', () => forkFrom(''));

  await refreshList();

  // Стенд переживает и перезагрузку страницы, и перезапуск сервера:
  // идентификатор в адресе, а дорожки — на диске.
  const params = new URLSearchParams(location.search);
  if (params.get('g')) {
    await openComparison(params.get('g'));
  } else if (params.get('c')) {
    await openSingle(params.get('c'));
  } else {
    openBlank();
  }
}

/* ---------- Журнал слева ---------- */

async function refreshList() {
  try {
    const data = await getJSON('/api/conversations');
    state.list = data.conversations || [];
    // Модель — свойство диалога, а не сервера: берём её из последнего.
    if (state.list.length) el.modelName.textContent = state.list[0].model;
  } catch (err) {
    showError('Список диалогов не прочитан: ' + err.message);
    state.list = [];
  }
  renderLedger();
}

// entries сводит диалоги в записи журнала: дорожки одного стенда — одна
// запись, одиночные диалоги — по записи на каждый.
function entries() {
  const byGroup = new Map();
  const out = [];
  for (const c of state.list) {
    if (!c.group) {
      out.push({ kind: 'single', id: c.id, title: c.title, updated: c.updated, items: [c] });
      continue;
    }
    if (!byGroup.has(c.group)) {
      const entry = { kind: 'comparison', id: c.group, title: c.title, updated: c.updated, items: [] };
      byGroup.set(c.group, entry);
      out.push(entry);
    }
    const entry = byGroup.get(c.group);
    entry.items.push(c);
    if (!entry.title && c.title) entry.title = c.title;
    if (c.updated > entry.updated) entry.updated = c.updated;
  }
  out.sort((a, b) => (a.updated < b.updated ? 1 : -1));
  return out;
}

function renderLedger() {
  const list = entries();
  el.conversations.innerHTML = '';
  el.conversationsEmpty.hidden = list.length > 0;

  for (const entry of list) {
    const li = document.createElement('li');
    li.className = 'ledger-item';
    if (state.view && state.view.id === entry.id) li.classList.add('active');
    li.tabIndex = 0;
    const open = () => (entry.kind === 'comparison' ? openComparison(entry.id) : openSingle(entry.id));
    li.addEventListener('click', (e) => {
      if (e.target.closest('button')) return;
      open();
    });
    li.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') open();
    });

    const title = document.createElement('div');
    title.className = 'title';
    title.textContent = entry.title || 'Без названия';
    li.appendChild(title);

    const drop = document.createElement('button');
    drop.type = 'button';
    drop.className = 'drop';
    drop.title = 'Удалить';
    drop.textContent = '×';
    drop.addEventListener('click', () => dropEntry(entry));
    li.appendChild(drop);

    const turns = entry.items.reduce((max, c) => Math.max(max, c.turns), 0);
    const cost = entry.items.reduce((sum, c) =>
      sum + (c.totals && c.totals.cost && c.totals.cost.known ? c.totals.cost.usd : 0), 0);
    const branches = entry.items.reduce((max, c) => Math.max(max, c.branches || 1), 1);
    const meta = document.createElement('div');
    meta.className = 'meta';
    const parts = [entry.kind === 'comparison'
      ? plural(entry.items.length, 'дорожка', 'дорожки', 'дорожек')
      : (MODES[entry.items[0].strategy] || {}).name || 'диалог'];
    parts.push(plural(turns, 'ход', 'хода', 'ходов'));
    if (branches > 1) parts.push(plural(branches, 'ветка', 'ветки', 'веток'));
    if (cost) parts.push(formatUSD(cost));
    meta.textContent = parts.join(' · ');
    li.appendChild(meta);

    el.conversations.appendChild(li);
  }
}

/* ---------- Открытие стенда ---------- */

function openBlank() {
  detachAll();
  state.view = null;
  history.replaceState(null, '', location.pathname);
  const single = state.draftKind === 'single';
  el.stageTitle.textContent = single ? 'Новый диалог' : 'Новый стенд';
  el.stageNote.textContent = single
    ? 'стратегия ' + ((MODES[state.draftMode] || {}).name || state.draftMode) + ', ветвление доступно после первого хода'
    : 'вопрос уйдёт трём дорожкам сразу: вся история, окно, факты';
  el.fileButton.hidden = true;
  el.deleteButton.hidden = true;
  el.switcher.hidden = true;
  el.branches.hidden = true;
  el.laneHeads.innerHTML = '';
  el.tape.innerHTML = '';
  el.tape.appendChild(el.tapeEmpty);
  el.tapeEmpty.hidden = false;
  el.sendButton.disabled = false;
  renderLedger();
}

async function openComparison(group) {
  let data;
  try {
    data = await getJSON('/api/comparisons/' + encodeURIComponent(group));
  } catch (err) {
    showError('Стенд не открылся: ' + err.message);
    openBlank();
    return;
  }
  showError('');
  setView({ kind: 'comparison', id: group, title: data.title, lanes: data.lanes || [] });
  history.replaceState(null, '', location.pathname + '?g=' + encodeURIComponent(group));
}

async function openSingle(id) {
  let data;
  try {
    data = await getJSON('/api/conversations/' + encodeURIComponent(id));
  } catch (err) {
    showError('Диалог не открылся: ' + err.message);
    openBlank();
    return;
  }
  showError('');
  setView({ kind: 'single', id, title: data.title, lanes: [data] });
  history.replaceState(null, '', location.pathname + '?c=' + encodeURIComponent(id));
}

// reopen перечитывает текущий стенд: после хода в дорожках появились
// новые сообщения, факты и счётчики.
async function reopen() {
  if (!state.view) return;
  if (state.view.kind === 'comparison') await openComparison(state.view.id);
  else await openSingle(state.view.id);
  await refreshList();
}

function setView(view) {
  state.view = view;
  el.stageTitle.textContent = view.title || 'Без названия';
  const lane = view.lanes[0] || {};
  const agent = state.agents.find((a) => a.key === lane.agentKey);
  const modes = view.lanes.map((l) => (MODES[l.strategy] || {}).name || l.strategy || 'стратегия по умолчанию');
  el.modelName.textContent = lane.model || el.modelName.textContent;
  const note = [agent ? agent.title : lane.agentKey, modes.join(' · ')];
  if (view.kind === 'single' && lane.branches > 1) {
    note.push(plural(lane.branches, 'ветка', 'ветки', 'веток') + ', сейчас «' + (lane.branchName || '—') + '»');
  }
  el.stageNote.textContent = note.filter(Boolean).join(' · ');
  el.fileButton.hidden = false;
  el.deleteButton.hidden = false;
  el.tapeEmpty.hidden = true;

  attachActive(view);
  renderStage();
  renderLedger();
}

// attachActive подписывается на ходы, которые идут прямо сейчас: у
// каждой дорожки свой поток событий.
function attachActive(view) {
  const wanted = new Set();
  view.lanes.forEach((lane) => {
    if (!lane.active) return;
    wanted.add(lane.active.id);
    if (!state.live[lane.active.id]) state.live[lane.active.id] = { view: lane.active, events: [] };
    if (!state.sources[lane.active.id]) attach(lane.active.id);
  });
  for (const id of Object.keys(state.sources)) {
    if (!wanted.has(id)) detach(id);
  }
  setBusy(wanted.size > 0);
}

// setBusy запрещает всё, что нельзя делать посреди хода: следующую
// реплику, ветвление и смену стратегии.
function setBusy(busy) {
  el.sendButton.disabled = busy;
  el.markButton.disabled = busy;
  el.forkButton.disabled = busy;
  for (const b of el.switcherButtons.querySelectorAll('button')) b.disabled = busy;
  for (const b of el.branchList.querySelectorAll('button')) b.disabled = busy;
}

/* ---------- Ход: отправка и поток событий ---------- */

async function ask() {
  const text = el.text.value.trim();
  if (!text) { showError('Введите реплику.'); return; }
  showError('');
  el.sendButton.disabled = true;

  try {
    if (!state.view) {
      const agent = state.draftAgent || (state.agents[0] && state.agents[0].key);
      if (state.draftKind === 'single') {
        const started = await postJSON('/api/conversations', { agent, text, strategy: state.draftMode });
        el.text.value = '';
        await refreshList();
        await openSingle(started.conversationId);
      } else {
        const started = await postJSON('/api/comparisons', { agent, text });
        el.text.value = '';
        await refreshList();
        await openComparison(started.group);
      }
    } else if (state.view.kind === 'comparison') {
      await postJSON('/api/comparisons/' + encodeURIComponent(state.view.id) + '/turns', { text });
      el.text.value = '';
      await openComparison(state.view.id);
    } else {
      await postJSON('/api/conversations/' + encodeURIComponent(state.view.id) + '/turns', { text });
      el.text.value = '';
      await openSingle(state.view.id);
    }
  } catch (err) {
    showError('Реплика не отправлена: ' + err.message);
    el.sendButton.disabled = false;
  }
}

function attach(turnId) {
  const source = new EventSource('/api/turns/' + encodeURIComponent(turnId) + '/events');
  state.sources[turnId] = source;

  source.addEventListener('snapshot', (e) => {
    const snap = JSON.parse(e.data);
    state.live[turnId] = { view: snap.view, events: snap.events || [] };
    renderStage();
  });
  source.addEventListener('log', (e) => {
    const live = state.live[turnId];
    if (!live) return;
    live.events.push(JSON.parse(e.data));
    renderStage();
  });
  source.addEventListener('state', (e) => {
    const live = state.live[turnId];
    if (!live) return;
    live.view = JSON.parse(e.data);
    renderStage();
  });
  source.addEventListener('done', () => {
    detach(turnId);
    // Ход дописан в файл; перечитываем стенд, когда замолчали все дорожки.
    if (Object.keys(state.sources).length === 0) reopen();
  });
  source.onerror = () => {
    const live = state.live[turnId];
    if (state.sources[turnId] && live && live.view.status === 'running') {
      showError('Поток событий прервался. Ход продолжается на сервере — обновите страницу.');
    }
    detach(turnId);
  };
}

function detach(turnId) {
  const source = state.sources[turnId];
  if (source) source.close();
  delete state.sources[turnId];
  setBusy(Object.keys(state.sources).length > 0);
}

function detachAll() {
  for (const id of Object.keys(state.sources)) detach(id);
  state.live = {};
}

/* ---------- Ветки и переключатель стратегий ---------- */

// applyDetail заменяет открытый одиночный диалог тем, что вернул сервер:
// ветвление и смена стратегии отвечают диалогом целиком, и перечитывать
// его отдельным запросом незачем.
function applyDetail(detail) {
  setView({ kind: 'single', id: detail.id, title: detail.title, lanes: [detail] });
  refreshList();
}

async function markCheckpoint() {
  if (!state.view || state.view.kind !== 'single') return;
  const name = prompt('Название точки сохранения (можно оставить пустым):', '');
  if (name === null) return;
  try {
    const data = await postJSON('/api/conversations/' + encodeURIComponent(state.view.id) + '/checkpoints', { name });
    showError('');
    applyDetail(data.conversation);
  } catch (err) {
    showError('Точка не поставлена: ' + err.message);
  }
}

async function forkFrom(checkpointId) {
  if (!state.view || state.view.kind !== 'single') return;
  const name = prompt('Название ветки (можно оставить пустым):', '');
  if (name === null) return;
  try {
    const data = await postJSON('/api/conversations/' + encodeURIComponent(state.view.id) + '/branches',
      { from: checkpointId, name });
    showError('');
    applyDetail(data.conversation);
  } catch (err) {
    showError('Ветка не создана: ' + err.message);
  }
}

async function switchBranch(branchId) {
  if (!state.view || state.view.kind !== 'single') return;
  try {
    const data = await postJSON('/api/conversations/' + encodeURIComponent(state.view.id) + '/switch',
      { branch: branchId });
    showError('');
    applyDetail(data);
  } catch (err) {
    showError('Ветка не переключилась: ' + err.message);
  }
}

async function switchStrategy(mode) {
  if (!state.view || state.view.kind !== 'single') return;
  try {
    const data = await postJSON('/api/conversations/' + encodeURIComponent(state.view.id) + '/strategy',
      { strategy: mode });
    showError('');
    applyDetail(data);
  } catch (err) {
    showError('Стратегия не переключилась: ' + err.message);
  }
}

// renderSwitcher — переключатель стратегий одиночного диалога. Сообщения
// при переключении не трогаются: меняется только то, что из них уходит
// модели на следующем ходе.
function renderSwitcher(lane) {
  el.switcherButtons.innerHTML = '';
  for (const mode of state.strategies) {
    const info = MODES[mode] || { name: mode, rule: '' };
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'ghost' + (lane.strategy === mode ? ' on' : '');
    button.style.setProperty('--tone', info.tone || 'var(--ink)');
    button.textContent = info.name;
    button.title = info.rule;
    button.addEventListener('click', () => switchStrategy(mode));
    el.switcherButtons.appendChild(button);
  }
  const info = MODES[lane.strategy] || { rule: '' };
  el.switcherRule.textContent = info.rule;
}

// renderBranches — дерево диалога: ветки чипами, под ними точки, от
// которых можно отпочковаться. Активная ветка — та, в которую уйдёт
// следующая реплика.
function renderBranches(lane) {
  const tree = lane.tree || [];
  el.branchList.innerHTML = '';
  for (const branch of tree) {
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'branch' + (branch.active ? ' on' : '');
    chip.addEventListener('click', () => { if (!branch.active) switchBranch(branch.id); });

    const name = document.createElement('b');
    name.textContent = branch.name;
    chip.appendChild(name);

    const meta = document.createElement('span');
    const parts = [plural(branch.pathTurns, 'ход', 'хода', 'ходов') + ' в пути'];
    if (branch.parent) {
      const parent = tree.find((b) => b.id === branch.parent);
      parts.push('от «' + (parent ? parent.name : '?') + '» после хода ' + branch.forkTurn);
    }
    if (branch.facts && branch.facts.count) {
      parts.push(plural(branch.facts.count, 'факт', 'факта', 'фактов'));
    }
    meta.textContent = parts.join(' · ');
    chip.appendChild(meta);
    el.branchList.appendChild(chip);
  }

  const points = lane.checkpoints || [];
  el.checkpointList.innerHTML = '';
  if (!points.length) {
    const hint = document.createElement('span');
    hint.className = 'hint';
    hint.textContent = 'Точек пока нет. «Ветка отсюда» поставит точку на текущем конце сама.';
    el.checkpointList.appendChild(hint);
    return;
  }
  const label = document.createElement('span');
  label.className = 'hint';
  label.textContent = 'Точки:';
  el.checkpointList.appendChild(label);
  for (const point of points) {
    const branch = (lane.tree || []).find((b) => b.id === point.branch);
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'checkpoint';
    button.textContent = point.name;
    button.title = 'Ветка от этой точки: ' + (branch ? '«' + branch.name + '», ' : '') +
      'ход ' + point.turn + ', ' + plural(point.at, 'сообщение', 'сообщения', 'сообщений') +
      (point.facts && point.facts.entries ? ', фактов ' + point.facts.entries.length : '');
    button.addEventListener('click', () => forkFrom(point.id));
    el.checkpointList.appendChild(button);
  }
}

/* ---------- Отрисовка стенда ---------- */

function renderStage() {
  const view = state.view;
  if (!view) return;
  const single = view.kind === 'single';
  el.switcher.hidden = !single;
  el.branches.hidden = !single;
  if (single) {
    renderSwitcher(view.lanes[0]);
    renderBranches(view.lanes[0]);
    setBusy(Object.keys(state.sources).length > 0);
  }
  el.laneHeads.style.setProperty('--lanes', view.lanes.length);
  renderLaneHeads(view);
  renderTape(view);
}

// laneTurns — ходы дорожки вместе с идущим прямо сейчас.
function laneTurns(lane) {
  const turns = (lane.turns || []).map((t) => ({
    id: t.id, status: t.status, user: t.user, reply: t.reply, error: t.error,
    branch: t.branch, totals: t.totals, context: t.context, events: t.events || [], live: false
  }));
  if (lane.active) {
    const live = state.live[lane.active.id];
    const view = live ? live.view : lane.active;
    turns.push({
      id: view.id, status: view.status, user: view.user, reply: view.reply, error: view.error,
      branch: view.branch, totals: view.totals, context: view.context,
      events: live ? live.events : [], live: true
    });
  }
  return turns;
}

// scaleOf — общий масштаб шкал: самое большое, что видела любая дорожка
// на последнем ходе, включая гипотетический размер без стратегии. Общий
// масштаб — единственное, что делает шкалы сравнимыми.
function scaleOf(view) {
  let max = 1;
  for (const lane of view.lanes) {
    const turns = laneTurns(lane);
    const last = turns[turns.length - 1];
    if (!last || !last.context) continue;
    const est = (last.context.estimate && last.context.estimate.total) || 0;
    max = Math.max(max, est, last.context.full || 0, last.context.linear || 0);
  }
  return max;
}

function renderLaneHeads(view) {
  const scale = scaleOf(view);
  el.laneHeads.innerHTML = '';

  view.lanes.forEach((lane) => {
    const mode = MODES[lane.strategy] ||
      { name: lane.strategy || 'стратегия по умолчанию', rule: '', tone: 'var(--ink)' };
    const head = document.createElement('div');
    head.className = 'lane-head';
    head.style.setProperty('--tone', mode.tone);

    const left = document.createElement('div');
    const name = document.createElement('h3');
    name.className = 'lane-name';
    name.textContent = mode.name;
    left.appendChild(name);

    const rule = document.createElement('p');
    rule.className = 'lane-rule';
    rule.textContent = mode.rule;
    left.appendChild(rule);

    const figures = document.createElement('div');
    figures.className = 'lane-figures';
    const usage = (lane.totals && lane.totals.usage) || {};
    figures.appendChild(figureLine('всего на вход', kilo(usage.prompt || 0) + ' ток.'));
    if (lane.totals && lane.totals.cost && lane.totals.cost.known) {
      figures.appendChild(figureLine('диалог стоил', formatUSD(lane.totals.cost.usd)));
    }
    if (lane.facts && lane.facts.calls) {
      figures.appendChild(figureLine('карточка',
        plural(lane.facts.count, 'факт', 'факта', 'фактов') + ', правок ' + lane.facts.version));
      if (lane.facts.cost && lane.facts.cost.known) {
        figures.appendChild(figureLine('карточка стоила',
          formatUSD(lane.facts.cost.usd) + ' за ' + plural(lane.facts.calls, 'запрос', 'запроса', 'запросов')));
      }
    }
    if (lane.branches > 1) {
      figures.appendChild(figureLine('ветка', lane.branchName + ' из ' +
        plural(lane.branches, 'ветки', 'веток', 'веток')));
    }
    left.appendChild(figures);
    head.appendChild(left);

    const turns = laneTurns(lane);
    head.appendChild(gauge(turns[turns.length - 1], scale));
    el.laneHeads.appendChild(head);
  });
}

function figureLine(label, value) {
  const span = document.createElement('span');
  span.textContent = label + ' ';
  const b = document.createElement('b');
  b.textContent = value;
  span.appendChild(b);
  return span;
}

// gauge — уровень занятого контекста на последнем ходе. Столбик растёт
// снизу и делится на то, из чего сложился запрос; пунктир поперёк —
// сколько ушло бы модели без стратегии, то есть высота столбика у дорожки
// «вся история».
function gauge(turn, scale) {
  const context = (turn && turn.context) || null;
  const wrap = document.createElement('div');
  wrap.className = 'gauge-wrap';

  const box = document.createElement('div');
  box.className = 'gauge';
  const est = (context && context.estimate) || {};
  const total = est.total || 0;

  const fill = document.createElement('div');
  fill.className = 'fill';
  fill.style.height = pct(total / scale);
  const base = (est.system || 0) + (est.tools || 0) + (est.user || 0);
  for (const [cls, value] of [['base', base], ['history', est.history || 0], ['facts', est.facts || 0]]) {
    if (!value) continue;
    const seg = document.createElement('span');
    seg.className = 'seg ' + cls;
    seg.style.height = pct(value / Math.max(1, total));
    seg.title = segTitle(cls) + ': ≈' + value + ' токенов';
    fill.appendChild(seg);
  }
  box.appendChild(fill);

  if (context && context.full && context.full > total) {
    const line = document.createElement('div');
    line.className = 'ghost-line';
    line.style.bottom = pct(context.full / scale);
    line.title = 'без стратегии ушло бы ≈' + context.full + ' токенов';
    box.appendChild(line);
  }
  if (context && context.linear && context.linear > (context.full || total)) {
    const line = document.createElement('div');
    line.className = 'ghost-line linear';
    line.style.bottom = pct(context.linear / scale);
    line.title = 'без ветвления, одной лентой, ушло бы ≈' + context.linear + ' токенов';
    box.appendChild(line);
  }
  box.title = total ? 'контекст последнего хода: ≈' + total + ' токенов' : 'ходов ещё не было';
  wrap.appendChild(box);

  const value = document.createElement('div');
  value.className = 'gauge-value';
  value.textContent = total ? '≈' + kilo(total) : '—';
  wrap.appendChild(value);
  return wrap;
}

function segTitle(cls) {
  if (cls === 'base') return 'промпт, инструменты и реплика';
  if (cls === 'history') return 'история дословно';
  return 'карточка фактов';
}

function renderTape(view) {
  const lanes = view.lanes.map(laneTurns);
  const beats = lanes.reduce((max, turns) => Math.max(max, turns.length), 0);
  el.tape.innerHTML = '';
  if (!beats) {
    el.tape.appendChild(el.tapeEmpty);
    el.tapeEmpty.hidden = false;
    return;
  }

  let shownBranch = null;
  for (let i = 0; i < beats; i++) {
    const asked = lanes.map((turns) => turns[i]).find(Boolean);

    // В одиночном диалоге лента — это путь текущей ветки, и место
    // ветвления надо показать: дальше идут ходы, которых в соседней ветке
    // нет.
    if (view.kind === 'single' && asked && asked.branch && asked.branch !== shownBranch) {
      if (shownBranch !== null) el.tape.appendChild(branchSeam(view.lanes[0], asked.branch));
      shownBranch = asked.branch;
    }

    const beat = document.createElement('div');
    beat.className = 'beat';

    const question = document.createElement('div');
    question.className = 'beat-q';
    const num = document.createElement('span');
    num.className = 'beat-num';
    num.textContent = (i + 1) + '.';
    question.appendChild(num);
    const text = document.createElement('p');
    text.className = 'beat-text';
    text.textContent = (asked && asked.user) || '';
    question.appendChild(text);
    beat.appendChild(question);

    const row = document.createElement('div');
    row.className = 'beat-lanes';
    row.style.setProperty('--lanes', view.lanes.length);
    view.lanes.forEach((lane, j) => {
      row.appendChild(laneCell(lane, lanes[j][i], i === beats - 1));
    });
    beat.appendChild(row);
    el.tape.appendChild(beat);
  }
}

// branchSeam — шов в ленте: с этого места разговор идёт в другой ветке.
function branchSeam(lane, branchId) {
  const branch = (lane.tree || []).find((b) => b.id === branchId);
  const seam = document.createElement('div');
  seam.className = 'seam';
  seam.textContent = branch
    ? 'дальше — ветка «' + branch.name + '»'
    : 'дальше — другая ветка';
  return seam;
}

// laneCell — что одна дорожка ответила на эту реплику: состояние, ответ,
// числа хода, журнал и — на последнем такте — её карточка фактов.
function laneCell(lane, turn, last) {
  const mode = MODES[lane.strategy] || { name: lane.strategy || 'диалог', tone: 'var(--ink)' };
  const cell = document.createElement('article');
  cell.className = 'lane-turn';
  cell.style.setProperty('--tone', mode.tone);

  if (!turn) {
    const nothing = document.createElement('p');
    nothing.className = 'status';
    nothing.textContent = 'хода не было';
    cell.appendChild(nothing);
    return cell;
  }

  // На узком экране дорожки идут одна под другой, и без подписи
  // непонятно, чей это ответ. На широком подпись прячется: там она уже
  // стоит в шапке над колонкой.
  const mark = document.createElement('p');
  mark.className = 'lane-mark';
  mark.textContent = mode.name || '';
  cell.appendChild(mark);

  const status = document.createElement('div');
  status.className = 'status';
  const dot = document.createElement('span');
  dot.className = 'dot' + (turn.status === 'running' ? ' running' : '');
  status.appendChild(dot);
  status.appendChild(document.createTextNode(statusLine(turn)));
  cell.appendChild(status);

  if (turn.reply) {
    const answer = document.createElement('div');
    answer.className = 'answer';
    answer.innerHTML = renderMarkdown(turn.reply);
    cell.appendChild(answer);
    // Длинные ответы подрезаем: рядом стоят ещё два, и сравнивают их по
    // началу. Целиком читают по требованию.
    if (turn.reply.length > 900) {
      const toggle = document.createElement('button');
      toggle.type = 'button';
      toggle.className = 'unclip';
      // Свернуть обратно так же важно, как развернуть: развёрнутый ответ
      // разъезжается с соседними дорожками, и сравнивать становится нечем.
      const paint = () => {
        const open = state.expanded.has(turn.id);
        answer.classList.toggle('clipped', !open);
        toggle.textContent = open ? 'свернуть ответ' : 'показать ответ целиком';
      };
      toggle.addEventListener('click', () => {
        if (state.expanded.has(turn.id)) state.expanded.delete(turn.id);
        else state.expanded.add(turn.id);
        paint();
      });
      paint();
      cell.appendChild(toggle);
    }
  } else if (turn.error) {
    const failed = document.createElement('p');
    failed.className = 'answer failed';
    failed.textContent = turn.error;
    cell.appendChild(failed);
  }

  const figures = turnFigures(turn);
  if (figures) cell.appendChild(figures);
  if (turn.events && turn.events.length) cell.appendChild(trail(turn));
  if (last && lane.factsCard && lane.factsCard.entries && lane.factsCard.entries.length) {
    cell.appendChild(card(lane.factsCard));
  }
  return cell;
}

function statusLine(turn) {
  if (turn.status === 'running') {
    const calls = turn.totals && turn.totals.llmCalls ? ', запросов к модели: ' + turn.totals.llmCalls : '';
    return 'идёт ход' + calls;
  }
  if (turn.status === 'failed') return 'ход не состоялся';
  const seconds = turn.totals && turn.totals.seconds ? turn.totals.seconds.toFixed(1) + ' с' : '';
  return 'ответ' + (seconds ? ' за ' + seconds : '');
}

// turnFigures — числа хода: во что обошёлся контекст и сколько сняла
// стратегия. Величины одинаковы во всех дорожках, чтобы их можно было
// читать поперёк ленты.
function turnFigures(turn) {
  const t = turn.totals || {};
  const u = t.usage || {};
  const c = turn.context || {};
  const parts = [];
  if (u.prompt) parts.push(['вход', kilo(u.prompt) + ' ток.']);
  if (u.completion) parts.push(['выход', kilo(u.completion) + ' ток.']);
  if (u.cacheHit) parts.push(['из кэша', Math.round(100 * u.cacheHit / Math.max(1, u.prompt)) + '%']);
  if (c.saved) parts.push(['стратегия сняла', '≈' + kilo(c.saved) + ' из ≈' + kilo(c.full)]);
  else if (c.dropped) parts.push(['выброшено окном', plural(c.dropped, 'сообщение', 'сообщения', 'сообщений')]);
  if (c.branchSaved) parts.push(['ветвление сняло', '≈' + kilo(c.branchSaved)]);
  if (t.cost && t.cost.known) parts.push(['стоил', formatUSD(t.cost.usd)]);
  if (!parts.length) return null;

  const box = document.createElement('div');
  box.className = 'turn-figures';
  for (const [label, value] of parts) {
    const span = document.createElement('span');
    span.textContent = label + ' ';
    const b = document.createElement('b');
    b.textContent = value;
    span.appendChild(b);
    box.appendChild(span);
  }
  return box;
}

// trail — журнал хода: что агент делал, пока отвечал.
function trail(turn) {
  const box = document.createElement('details');
  box.className = 'trail';
  const summary = document.createElement('summary');
  summary.textContent = 'журнал хода: ' + plural(turn.events.length, 'запись', 'записи', 'записей');
  box.appendChild(summary);
  if (turn.live) box.open = true;

  const body = document.createElement('div');
  body.className = 'trail-body';
  for (const ev of turn.events) {
    body.appendChild(trailItem(ev, turn));
  }
  box.appendChild(body);
  return box;
}

function trailItem(ev, turn) {
  const item = document.createElement('div');
  item.className = 'trail-item' + (ev.kind.endsWith('error') ? ' err' : '');

  const at = document.createElement('span');
  at.className = 'at';
  at.textContent = '+' + offsetSeconds(ev.time, turn).toFixed(1) + 'с';
  item.appendChild(at);

  const what = document.createElement('div');
  what.className = 'what';
  const kind = document.createElement('span');
  kind.className = 'kind';
  kind.textContent = KIND_LABEL[ev.kind] || ev.kind;
  what.appendChild(kind);
  what.appendChild(document.createTextNode(ev.kind === 'prompt' ? promptLine(ev) : ev.title));

  const numbers = eventNumbers(ev);
  if (numbers) {
    const line = document.createElement('span');
    line.className = 'numbers';
    line.textContent = numbers;
    what.appendChild(line);
  }

  const detail = ev.kind === 'prompt' ? promptDetail(ev) : ev.detail;
  if (detail) {
    const more = document.createElement('details');
    const label = document.createElement('summary');
    label.textContent = 'подробности';
    more.appendChild(label);
    const pre = document.createElement('pre');
    pre.textContent = detail;
    more.appendChild(pre);
    what.appendChild(more);
  }
  item.appendChild(what);
  return item;
}

// promptLine — из чего сложился запрос на старте хода. В журнале это
// главная строка: она объясняет, почему дорожки стоят по-разному.
function promptLine(ev) {
  const p = parsePrompt(ev);
  if (!p) return ev.title;
  const est = p.estimate || {};
  const parts = ['≈' + (est.total || 0) + ' ток.'];
  if (est.facts) parts.push('карточка ≈' + est.facts);
  parts.push('история ≈' + (est.history || 0) + ' (' + p.history + ' сообщ.)');
  if (p.dropped) parts.push('не ушло ' + p.dropped + ' сообщ.');
  if (p.linear && p.full && p.linear > p.full) parts.push('без ветвления было бы ≈' + p.linear);
  return parts.join(', ');
}

function promptDetail(ev) {
  const p = parsePrompt(ev);
  if (!p) return ev.detail;
  const blocks = ['Сообщение system:\n' + p.system];
  if (p.facts) blocks.push('Карточка фактов, отдельным сообщением system:\n' + p.facts);
  blocks.push('Новое сообщение user:\n' + p.user);
  return blocks.join('\n\n———\n\n');
}

function parsePrompt(ev) {
  try { return JSON.parse(ev.detail); } catch (_) { return null; }
}

function eventNumbers(ev) {
  const parts = [];
  if (ev.tokens && ev.tokens.estimated && ev.kind !== 'prompt') {
    let text = '≈' + ev.tokens.estimated;
    if (ev.tokens.actual) {
      text += ' против ' + ev.tokens.actual + ' (' + signedPct(ev.tokens.errorPct || 0) + ')';
    }
    parts.push(text);
  }
  if (ev.usage) parts.push(ev.usage.prompt + '→' + ev.usage.completion + ' ток.');
  if (ev.cost && ev.cost.known) parts.push(formatUSD(ev.cost.usd));
  if (ev.seconds) parts.push(ev.seconds.toFixed(1) + ' с');
  return parts.join(' · ');
}

// card — карточка фактов дорожки: всё, что она помнит о начале разговора,
// по строке на факт. Модель видит именно её, а не начало списка сообщений.
function card(memory) {
  const box = document.createElement('details');
  box.className = 'card';
  const summary = document.createElement('summary');
  summary.textContent = 'карточка фактов: ' +
    plural(memory.entries.length, 'запись', 'записи', 'записей') + ', правок ' + memory.version;
  box.appendChild(summary);

  const table = document.createElement('table');
  table.className = 'card-table';
  for (const entry of memory.entries) {
    const tr = document.createElement('tr');
    const key = document.createElement('th');
    key.textContent = entry.key;
    tr.appendChild(key);
    const value = document.createElement('td');
    value.textContent = entry.value;
    tr.appendChild(value);
    const when = document.createElement('td');
    when.className = 'when';
    when.textContent = entry.since === entry.turn ? 'ход ' + entry.turn : entry.since + '→' + entry.turn;
    when.title = 'записан на ходе ' + entry.since + ', последняя правка на ходе ' + entry.turn;
    tr.appendChild(when);
    table.appendChild(tr);
  }
  box.appendChild(table);
  return box;
}

/* ---------- Файлы и удаление ---------- */

async function showFiles() {
  if (!state.view) return;
  el.fileTabs.innerHTML = '';
  state.fileLane = Math.min(state.fileLane, state.view.lanes.length - 1);

  state.view.lanes.forEach((lane, i) => {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'ghost' + (i === state.fileLane ? ' on' : '');
    button.textContent = (MODES[lane.strategy] || {}).name || lane.strategy || 'диалог';
    button.addEventListener('click', () => { state.fileLane = i; showFiles(); });
    el.fileTabs.appendChild(button);
  });

  const lane = state.view.lanes[state.fileLane];
  try {
    const data = await getJSON('/api/conversations/' + encodeURIComponent(lane.id) + '/file');
    el.filePath.textContent = data.path;
    el.fileBody.textContent = data.json;
  } catch (err) {
    el.filePath.textContent = '—';
    el.fileBody.textContent = 'файл не прочитан: ' + err.message;
  }
  if (!el.fileDialog.open) el.fileDialog.showModal();
}

function dropCurrent() {
  if (!state.view) return;
  dropEntry({
    kind: state.view.kind,
    id: state.view.id,
    title: state.view.title,
    items: state.view.lanes
  });
}

async function dropEntry(entry) {
  const what = entry.kind === 'comparison'
    ? 'Удалить стенд «' + (entry.title || 'без названия') + '» вместе со всеми дорожками?'
    : 'Удалить диалог «' + (entry.title || 'без названия') + '» вместе со всеми ветками?';
  if (!confirm(what + ' Файлы на диске тоже будут удалены.')) return;

  try {
    for (const item of entry.items) {
      await fetchJSON('/api/conversations/' + encodeURIComponent(item.id), { method: 'DELETE' });
    }
  } catch (err) {
    showError('Не удалось удалить: ' + err.message);
    return;
  }
  if (state.view && state.view.id === entry.id) openBlank();
  await refreshList();
}

/* ---------- Мелочи ---------- */

function strategyNote(p) {
  if (!p) return '—';
  const parts = [plural(p.keep || 8, 'сообщение', 'сообщения', 'сообщений') + ' дословно'];
  const f = p.facts || {};
  parts.push('карточка до ' + (f.maxFacts || 24) + ' фактов');
  return parts.join(' · ');
}

// renderPicker — что заводим: стенд или один диалог, каким агентом и (для
// одиночного) с какой стратегии начинаем.
function renderPicker() {
  const kinds = [
    { key: 'comparison', name: 'Стенд из трёх дорожек', about: 'Один вопрос уходит трём диалогам сразу: вся история, окно, факты. Ответы и числа стоят рядом.' },
    { key: 'single', name: 'Один диалог', about: 'Одна дорожка, зато её можно ветвить: точка сохранения, два продолжения, переключение между ними. Стратегию можно менять на ходу.' }
  ];
  fillChoice(el.kindChoice, 'kind', kinds.map((k) => ({ value: k.key, name: k.name, about: k.about })),
    state.draftKind);
  fillChoice(el.agentChoice, 'agent',
    state.agents.map((a) => ({ value: a.key, name: a.title, about: a.description })),
    state.draftAgent || (state.agents[0] && state.agents[0].key));
  fillChoice(el.strategyChoice, 'strategy',
    state.strategies.map((m) => ({
      value: m,
      name: (MODES[m] || {}).name || m,
      about: (MODES[m] || {}).rule || ''
    })), state.draftMode);

  const paint = () => {
    const kind = el.kindChoice.querySelector('input:checked');
    el.singleStrategy.hidden = !kind || kind.value !== 'single';
  };
  for (const input of el.kindChoice.querySelectorAll('input')) {
    input.addEventListener('change', paint);
  }
  paint();
}

function fillChoice(host, name, options, checked) {
  host.innerHTML = '';
  options.forEach((option, i) => {
    const label = document.createElement('label');
    const input = document.createElement('input');
    input.type = 'radio';
    input.name = name;
    input.value = option.value;
    if (option.value === checked || (checked === undefined && i === 0)) input.checked = true;
    label.appendChild(input);

    const title = document.createElement('span');
    title.className = 'name';
    title.textContent = option.name;
    label.appendChild(title);

    if (option.about) {
      const about = document.createElement('span');
      about.className = 'about';
      about.textContent = option.about;
      label.appendChild(about);
    }
    host.appendChild(label);
  });
}

function openPicker() {
  if (state.agents.length) el.newDialog.showModal();
  else openBlank();
}

function offsetSeconds(time, turn) {
  const first = turn.events && turn.events[0] ? turn.events[0].time : time;
  return Math.max(0, (new Date(time) - new Date(first)) / 1000);
}

function renderMarkdown(text) {
  if (window.marked) return marked.parse(text || '');
  return escapeHTML(text || '');
}

function escapeHTML(s) {
  const div = document.createElement('div');
  div.textContent = s;
  return div.innerHTML;
}

function pct(share) {
  const value = Math.max(0, Math.min(1, share || 0));
  return (value * 100).toFixed(2) + '%';
}

function kilo(n) {
  if (n >= 10000) return (n / 1000).toFixed(1) + ' тыс.';
  return String(n);
}

function formatUSD(v) {
  if (v >= 0.01) return '$' + v.toFixed(4);
  return '$' + v.toFixed(6);
}

function signedPct(v) {
  return (v > 0 ? '+' : '') + v.toFixed(0) + '%';
}

function plural(n, one, few, many) {
  let word = many;
  if (n % 10 === 1 && n % 100 !== 11) word = one;
  else if (n % 10 >= 2 && n % 10 <= 4 && (n % 100 < 10 || n % 100 >= 20)) word = few;
  return n + ' ' + word;
}

function formatTime(date) {
  const pad = (n) => String(n).padStart(2, '0');
  return pad(date.getDate()) + '.' + pad(date.getMonth() + 1) + ' ' +
    pad(date.getHours()) + ':' + pad(date.getMinutes()) + ':' + pad(date.getSeconds());
}

function showError(text) {
  el.error.textContent = text;
  el.error.hidden = !text;
}

async function getJSON(url) {
  return fetchJSON(url, {});
}

async function postJSON(url, body) {
  return fetchJSON(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
}

async function fetchJSON(url, options) {
  const resp = await fetch(url, options);
  const text = await resp.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (_) { data = null; }
  if (!resp.ok) throw new Error((data && data.error) || ('HTTP ' + resp.status));
  return data;
}

init();

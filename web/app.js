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
  // Что показывает лента: путь текущей ветки (то, что видит модель) или
  // весь диалог одной лентой (то, чем он был бы без ветвления). Это и
  // есть сравнение дерева с линейным диалогом.
  tapeMode: 'branch',
  // Ходы, у которых ответ развёрнут целиком. Лента перерисовывается на
  // каждое событие идущего хода, и без этого развёрнутый ответ схлопывался
  // бы сам собой посреди чтения.
  expanded: new Set()
};

const el = {};
for (const id of [
  'model-name', 'strategy-note', 'history-dir', 'server-started',
  'new-button', 'conversations', 'conversations-empty',
  'stage-title', 'stage-note', 'stand-button', 'file-button', 'delete-button',
  'switcher', 'switcher-buttons', 'switcher-rule',
  'nav-block', 'nav-note', 'tree', 'tree-hint', 'mark-button', 'fork-button',
  'views', 'view-branch', 'view-linear', 'view-note',
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
  el.standButton.addEventListener('click', () => {
    const lane = state.view && state.view.lanes[0];
    if (lane && lane.group) openComparison(lane.group);
  });
  el.markButton.addEventListener('click', markCheckpoint);
  el.forkButton.addEventListener('click', () => forkFrom(''));
  el.viewBranch.addEventListener('click', () => setTapeMode('branch'));
  el.viewLinear.addEventListener('click', () => setTapeMode('linear'));

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
  el.standButton.hidden = true;
  el.fileButton.hidden = true;
  el.deleteButton.hidden = true;
  el.switcher.hidden = true;
  el.navBlock.hidden = true;
  el.views.hidden = true;
  state.tapeMode = 'branch';
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
  // Дорожку стенда можно открыть отдельно — тогда у неё появляются ветки
  // и переключатель. Обратная дорога нужна там же: иначе стенд ищется
  // только в журнале слева или по адресу.
  el.standButton.hidden = !(view.kind === 'single' && lane.group);
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
  // Переход к месту в ленте ходу не мешает — блокируем только то, что
  // меняет диалог: переключение веток и ветвление от точки.
  for (const b of el.tree.querySelectorAll('.tree-branch, .point-fork')) b.disabled = busy;
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
  // Про «это не ветка» сказано прямо в вопросе: человек, который хочет
  // ветку, обычно жмёт первую кнопку и вписывает в неё имя ветки.
  const name = prompt(
    'Точка помечает место в разговоре — ветку от неё можно отвести потом, кнопкой «⑂».\n' +
    'Название точки (можно оставить пустым):', '');
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

// renderTree — дерево диалога на пульте слева: ветки с отступом по
// глубине, под каждой её точки, а под точкой — ветки, выросшие из неё.
// Пульт липнет к верху окна, поэтому ветвиться и переключаться можно с
// любого места ленты, не прокручивая её вверх.
function renderTree(lane) {
  const tree = lane.tree || [];
  const points = lane.checkpoints || [];
  el.tree.innerHTML = '';

  const walk = (branch, depth) => {
    el.tree.appendChild(branchRow(branch, depth, tree));

    const own = points.filter((p) => p.branch === branch.id).sort((a, b) => a.at - b.at);
    const kids = tree.filter((b) => b.parent === branch.id);
    const placed = new Set();
    // Ветка выросла из точки — значит, и стоять должна под ней: так
    // видно не просто список, а место, где разговор раздвоился.
    for (const point of own) {
      el.tree.appendChild(pointRow(point, depth + 1, branch));
      for (const kid of kids.filter((k) => k.forkAt === point.at)) {
        placed.add(kid.id);
        walk(kid, depth + 2);
      }
    }
    for (const kid of kids.filter((k) => !placed.has(k.id))) walk(kid, depth + 1);
  };

  // Корень — ветка без родителя; на всякий случай и та, чей родитель
  // потерялся: файл диалога можно поправить руками.
  for (const branch of tree) {
    if (!branch.parent || !tree.some((b) => b.id === branch.parent)) walk(branch, 0);
  }

  el.navNote.textContent = [
    plural(tree.length, 'ветка', 'ветки', 'веток'),
    plural(points.length, 'точка', 'точки', 'точек')
  ].join(' · ');

  el.treeHint.textContent = tree.length > 1
    ? 'Клик по ветке — перейти в неё. Клик по ⑂ — показать это место в ленте, кнопка «ветка» рядом — отвести отсюда новую.'
    : 'Нажмите «⑂ Ветка отсюда» — разговор раздвоится с этого места, и в каждой ветке он пойдёт своей дорогой.';
}

// branchRow — строка ветки: имя, длина пути и место, откуда выросла.
function branchRow(branch, depth, tree) {
  const row = document.createElement('button');
  row.type = 'button';
  row.className = 'tree-branch' + (branch.active ? ' on' : '');
  row.style.setProperty('--depth', depth);
  row.title = branch.active
    ? 'Здесь идёт разговор: следующая реплика уйдёт в эту ветку'
    : 'Перейти в эту ветку — следующая реплика уйдёт в неё';
  row.addEventListener('click', () => { if (!branch.active) switchBranch(branch.id); });

  const name = document.createElement('b');
  name.textContent = branch.name;
  row.appendChild(name);

  const meta = document.createElement('span');
  const parts = [plural(branch.pathTurns, 'ход', 'хода', 'ходов') + ' в пути'];
  if (branch.parent) {
    const parent = tree.find((b) => b.id === branch.parent);
    parts.push('своих ' + branch.turns);
    if (parent) parts.push('от «' + parent.name + '»');
  }
  if (branch.facts && branch.facts.count) {
    parts.push(plural(branch.facts.count, 'факт', 'факта', 'фактов'));
  }
  meta.textContent = parts.join(' · ');
  row.appendChild(meta);
  return row;
}

// pointRow — строка точки. У точки две роли, и раньше они спорили: клик
// и переключал взгляд, и заводил ветку. Теперь роли разведены — по самой
// строке переходят к месту в ленте, кнопкой рядом отводят ветку.
function pointRow(point, depth, branch) {
  const row = document.createElement('div');
  row.className = 'tree-point';
  row.style.setProperty('--depth', depth);

  const where = 'Ветка «' + branch.name + '», после хода ' + point.turn + ' (' +
    plural(point.at, 'сообщение', 'сообщения', 'сообщений') +
    (point.facts && point.facts.entries ? ', фактов ' + point.facts.entries.length : '') + ')';

  const go = document.createElement('button');
  go.type = 'button';
  go.className = 'point-go';
  go.title = 'Показать это место в ленте. ' + where;
  go.addEventListener('click', () => showPlace(point));

  const mark = document.createElement('span');
  mark.className = 'fork';
  mark.textContent = '⑂';
  go.appendChild(mark);

  const name = document.createElement('span');
  name.className = 'point-name';
  name.textContent = point.name;
  go.appendChild(name);
  row.appendChild(go);

  const fork = document.createElement('button');
  fork.type = 'button';
  fork.className = 'point-fork';
  fork.textContent = 'ветка';
  fork.title = 'Отвести отсюда новую ветку и перейти в неё. ' + where;
  fork.addEventListener('click', () => forkFrom(point.id));
  row.appendChild(fork);
  return row;
}

// showPlace прокручивает ленту к месту, где стоит точка. Если это место
// в соседней ветке, в пути его нет — тогда переключаемся на ленту «весь
// диалог»: там есть все ходы, и показать можно любое место.
function showPlace(point) {
  if (scrollToPlace(point)) return;

  const lane = state.view && state.view.lanes[0];
  if (state.tapeMode !== 'linear' && lane && (lane.branches || 1) > 1) {
    state.tapeMode = 'linear';
    renderStage();
    if (scrollToPlace(point)) return;
  }

  // Не нашли. Причина одна из двух, и обе стоит назвать: место в другой
  // ветке — или точка старая и не помнит своего хода (метку начали
  // сохранять не сразу).
  const branch = ((lane && lane.tree) || []).find((b) => b.id === point.branch);
  const where = branch ? 'Это место в ветке «' + branch.name + '»' : 'Это место в другой ветке';
  showError(point.after
    ? where + ' — перейдите в неё, чтобы увидеть.'
    : where + ', а сама точка поставлена до того, как появилась навигация, и не помнит своего хода: ' +
      'перейдите в ветку и найдите ход ' + point.turn + '.');
}

// scrollToPlace ищет такт по метке хода, а у старых точек, которые её не
// помнят, — по номеру такта. Возвращает, нашлось ли место.
function scrollToPlace(point) {
  let beat = null;
  if (point.after) beat = el.tape.querySelector('[data-turn="' + point.after + '"]');
  if (!beat && point.turn > 0 && state.tapeMode === 'branch') {
    beat = el.tape.querySelector('[data-beat="' + point.turn + '"]');
  }
  if (!beat) return false;

  showError('');
  beat.scrollIntoView({ behavior: reducedMotion() ? 'auto' : 'smooth', block: 'center' });
  // Подсветка держится пару секунд: после прокрутки на длинной ленте
  // глазу надо за что-то зацепиться.
  for (const lit of el.tape.querySelectorAll('.beat.lit')) lit.classList.remove('lit');
  beat.classList.add('lit');
  window.setTimeout(() => beat.classList.remove('lit'), 2200);
  return true;
}

function reducedMotion() {
  return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

/* ---------- Сравнение дерева с линейным диалогом ---------- */

// renderViews — две вкладки над лентой. «Путь ветки» — то, что видит
// модель; «Весь диалог лентой» — тот же разговор, каким он был бы, если
// бы ветвиться было нельзя. Пока ветка одна, сравнивать нечего, и
// вкладок нет.
function renderViews(lane) {
  const linear = lane.linear || [];
  const branched = (lane.branches || 1) > 1;
  el.views.hidden = !branched;
  if (!branched) {
    state.tapeMode = 'branch';
    return;
  }

  el.viewBranch.classList.toggle('on', state.tapeMode === 'branch');
  el.viewLinear.classList.toggle('on', state.tapeMode === 'linear');
  el.viewBranch.textContent = 'Путь ветки «' + (lane.branchName || '—') + '»';
  el.viewLinear.textContent = 'Весь диалог лентой';

  const pathTurns = (lane.turns || []).length;
  const pathMessages = (lane.messages || []).length;
  const hidden = (lane.linearMessages || 0) - pathMessages;
  const parts = [];
  if (state.tapeMode === 'branch') {
    parts.push('Модель видит путь ветки: ' + plural(pathTurns, 'ход', 'хода', 'ходов') +
      ', ' + plural(pathMessages, 'сообщение', 'сообщения', 'сообщений') + '.');
    if (hidden > 0) {
      parts.push('В соседних ветках — ещё ' + plural(hidden, 'сообщение', 'сообщения', 'сообщений') +
        '; в этот контекст они не попадают вовсе.');
    }
  } else {
    parts.push('Так выглядел бы разговор, если бы ветвиться было нельзя: ' +
      plural(linear.length, 'ход', 'хода', 'ходов') + ', ' +
      plural(lane.linearMessages || 0, 'сообщение', 'сообщения', 'сообщений') +
      ' в одном контексте, все ветки подряд.');
    parts.push('Модель этого не видела ни разу: ходы шли по веткам.');
  }
  const money = linearCost(lane);
  if (money) parts.push(money);
  el.viewNote.textContent = parts.join(' ');
}

// linearCost — во что обошёлся бы последний ход без ветвления. Счётчик
// живёт в самом ходе: агент считает эту оценку даром на каждом ходе.
// Пока вторая ветка не заведена, сравнивать не с чем, и счётчика нет.
function linearCost(lane) {
  const turns = lane.turns || [];
  for (let i = turns.length - 1; i >= 0; i--) {
    const c = turns[i].context || {};
    if (c.linear && c.estimate && c.estimate.total) {
      return 'На последнем ходе модель получила ≈' +
        plural(c.estimate.total, 'токен', 'токена', 'токенов') +
        ' вместо ≈' + c.linear + ', которые ушли бы одной лентой.';
    }
  }
  return '';
}

function setTapeMode(mode) {
  if (state.tapeMode === mode) return;
  state.tapeMode = mode;
  renderStage();
}

/* ---------- Отрисовка стенда ---------- */

function renderStage() {
  const view = state.view;
  if (!view) return;
  const single = view.kind === 'single';
  el.switcher.hidden = !single;
  el.navBlock.hidden = !single;
  if (single) {
    renderSwitcher(view.lanes[0]);
    renderTree(view.lanes[0]);
    renderViews(view.lanes[0]);
    setBusy(Object.keys(state.sources).length > 0);
  } else {
    el.views.hidden = true;
  }
  el.laneHeads.style.setProperty('--lanes', view.lanes.length);
  renderLaneHeads(view);
  renderTape(view);
}

// laneTurns — ходы дорожки вместе с идущим прямо сейчас.
function laneTurns(lane) {
  // messages — сколько сообщений ход добавил в ветку. По ним лента
  // находит границу окна: счётчик хода считает отрезанное в сообщениях,
  // а лента нарисована ходами.
  const turns = (lane.turns || []).map((t) => ({
    id: t.id, status: t.status, user: t.user, reply: t.reply, error: t.error,
    branch: t.branch, messages: t.messages, totals: t.totals, context: t.context,
    events: t.events || [], live: false
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
    // Счётчики дорожки — это всё, что она потратила, вместе с ведением
    // карточки: извлекатель — такой же запрос к модели, и прятать его
    // цену в отдельную строку «сверх» значило бы занижать счёт. Поэтому
    // карточка идёт не рядом с итогом, а внутри него: «из них».
    const usage = (lane.totals && lane.totals.usage) || {};
    figures.appendChild(figureLine('всего на вход', kilo(usage.prompt || 0) + ' ток.'));
    if (lane.totals && lane.totals.cost && lane.totals.cost.known) {
      figures.appendChild(figureLine('дорожка стоила', formatUSD(lane.totals.cost.usd)));
    }
    if (lane.facts && lane.facts.calls) {
      figures.appendChild(figureLine('карточка',
        plural(lane.facts.count, 'факт', 'факта', 'фактов') + ', правок ' + lane.facts.version));
      if (lane.facts.cost && lane.facts.cost.known) {
        figures.appendChild(figureLine('из них на карточку',
          formatUSD(lane.facts.cost.usd) + ' за ' + plural(lane.facts.calls, 'запрос', 'запроса', 'запросов')));
      }
    }
    if (lane.branches > 1) {
      figures.appendChild(figureLine('ветка', lane.branchName + ' из ' +
        plural(lane.branches, 'ветки', 'веток', 'веток')));
    }
    left.appendChild(figures);

    // Дорожка стенда — настоящий диалог со своим файлом, и её можно
    // открыть отдельно: ветвление и переключатель стратегий работают
    // только в одиночном виде, а начатый на стенде разговор бросать
    // ради них не хочется.
    if (view.kind === 'comparison') {
      const open = document.createElement('button');
      open.type = 'button';
      open.className = 'lane-open';
      open.textContent = 'открыть отдельно →';
      open.title = 'Открыть эту дорожку как одиночный диалог: там доступны ветки и переключатель стратегий. ' +
        'Ходы, сделанные отдельно, в остальные дорожки не попадут — стенд перестанет идти в ногу.';
      open.addEventListener('click', () => openSingle(lane.id));
      left.appendChild(open);
    }
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
  // В режиме ленты дорожка одна и в ней все ходы всех веток подряд: так
  // выглядел бы разговор, если бы ветвиться было нельзя. Карточку фактов
  // под последним ходом в этом режиме не показываем — она принадлежит
  // текущей ветке, а лента к ветке не привязана.
  const single = view.kind === 'single';
  const linear = single && state.tapeMode === 'linear';
  const lanes = linear
    ? [(view.lanes[0].linear || []).map((t) => ({ ...t, events: [], live: false }))]
    : view.lanes.map(laneTurns);
  const beats = lanes.reduce((max, turns) => Math.max(max, turns.length), 0);
  el.tape.innerHTML = '';
  if (!beats) {
    el.tape.appendChild(el.tapeEmpty);
    el.tapeEmpty.hidden = false;
    return;
  }

  // Граница окна считается по одной дорожке: на стенде у трёх дорожек
  // она своя, и одна черта поперёк ленты врала бы сразу про две. В ленте
  // «весь диалог» её тоже нет — тот поток модель не видела ни разу.
  const bound = single && !linear ? windowBoundary(lanes[0]) : null;

  let shownBranch = null;
  let shownMode = null;
  for (let i = 0; i < beats; i++) {
    const asked = lanes.map((turns) => turns[i]).find(Boolean);

    // В одиночном диалоге лента — это путь текущей ветки, и место
    // ветвления надо показать: дальше идут ходы, которых в соседней ветке
    // нет.
    if (single && asked && asked.branch && asked.branch !== shownBranch) {
      // В ленте швом подписан каждый кусок, включая первый: она для того
      // и нужна, чтобы видеть, где чей вариант. В пути ветки первый шов
      // лишний — разговор с него и начинается.
      if (shownBranch !== null || linear) {
        el.tape.appendChild(branchSeam(view.lanes[0], asked.branch, linear));
      }
      shownBranch = asked.branch;
    }

    // Стратегию можно переключить посреди разговора, и тогда соседние
    // ходы сравнивать нельзя: у них разный контекст. Каждый ход помнит,
    // по какой стратегии шёл, — на смене ставим шов, иначе подмену
    // замечаешь только по числам, и то не сразу.
    const mode = asked && asked.context && asked.context.mode;
    if (single && mode && mode !== shownMode) {
      if (shownMode !== null) el.tape.appendChild(modeSeam(mode));
      shownMode = mode;
    }

    const beat = document.createElement('div');
    beat.className = 'beat';
    // Метка хода нужна навигации: по ней точка находит своё место в
    // ленте. Номер такта для этого не годится — в ленте «весь диалог»
    // ходы идут не по одной ветке.
    if (asked && asked.id) beat.dataset.turn = asked.id;
    beat.dataset.beat = i + 1;

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
      row.appendChild(laneCell(lane, lanes[j][i], i === beats - 1 && !linear));
    });
    beat.appendChild(row);
    el.tape.appendChild(beat);

    // Граница окна: выше неё модель на последнем ходе не видела ничего.
    // Черта двигается — с каждым новым ходом она опускается.
    if (bound && bound.after === i) el.tape.appendChild(windowMark(bound));
  }
}

// windowBoundary — где на последнем ходе прошла граница окна. Отрезанное
// считается в сообщениях, а лента нарисована ходами, поэтому сообщения
// набираются ходами, пока не наберётся отрезанное.
function windowBoundary(turns) {
  let last = null;
  for (let i = turns.length - 1; i >= 0; i--) {
    const c = turns[i].context;
    if (c && c.mode) { last = c; break; }
  }
  if (!last || !last.dropped) return null;

  let seen = 0;
  for (let i = 0; i < turns.length; i++) {
    seen += turns[i].messages || 0;
    if (seen >= last.dropped) return { after: i, dropped: last.dropped, mode: last.mode };
  }
  return null;
}

// windowMark — сама черта. Не шов слева, а линейка поперёк ленты: это не
// событие разговора, а край того, что видит модель.
function windowMark(bound) {
  const info = MODES[bound.mode] || {};
  const mark = document.createElement('div');
  mark.className = 'window-mark';
  mark.style.setProperty('--tone', info.tone || 'var(--muted)');
  const count = plural(bound.dropped, 'сообщение', 'сообщения', 'сообщений');
  mark.textContent = bound.mode === 'facts'
    ? 'выше дословной истории нет: ' + count + ' заменено карточкой фактов'
    : 'выше модель не видит ничего: ' + count + ' отрезано окном';
  mark.title = 'Граница окна на последнем ходе. С каждым новым ходом она опускается ниже: ' +
    'окно держит постоянный размер, а разговор растёт.';
  return mark;
}

// modeSeam — шов смены стратегии: с этого места модель получает историю
// по другим правилам. Красится цветом той стратегии, на которую перешли,
// — тем же, каким она подписана на пульте и на стенде.
function modeSeam(mode) {
  const info = MODES[mode] || { name: mode, rule: '' };
  const seam = document.createElement('div');
  seam.className = 'seam mode';
  seam.style.setProperty('--tone', info.tone || 'var(--ink)');
  seam.textContent = 'дальше — стратегия «' + info.name + '»';
  seam.title = info.rule ? 'С этого хода: ' + info.rule : '';
  return seam;
}

// branchSeam — шов в ленте: с этого места разговор идёт в другой ветке.
// В режиме «весь диалог лентой» это заголовок куска, в пути ветки —
// отметка на месте ветвления.
function branchSeam(lane, branchId, head) {
  const branch = (lane.tree || []).find((b) => b.id === branchId);
  const name = branch ? '«' + branch.name + '»' : 'другая ветка';
  const seam = document.createElement('div');
  seam.className = 'seam' + (head ? ' head' : '');
  seam.textContent = head ? 'ветка ' + name : 'дальше — ветка ' + name;
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

// DB 서버와 그 아래 관리 대상 DB 화면.
//
// 서버로 묶어 보여주는 이유: 접속 정보와 자격증명은 서버의 것이고 그 아래 DB들이
// 함께 쓴다. 평평한 목록으로 두면 같은 호스트가 열 줄 반복되고, 비밀번호를 어디서
// 고쳐야 하는지가 보이지 않는다.
import { api } from '../core/api.js';
import { state, kindInfo, kindLabel } from '../core/store.js';
import {
  h, mount, icon, field, input, select, textarea, checkbox, spinner, emptyState,
  pageHeader, openModal, confirmDialog, toast, toastError, badge, envBadge,
  levelBadge, relativeTime,
} from '../core/ui.js';
import { dbLogo } from '../core/dblogo.js';
import { errorPanel } from './users.js';

// 접힌 서버를 기억한다. 다시 그릴 때마다 전부 펴지면 방금 접은 것이 되살아난다.
const collapsed = new Set();

export async function renderConnections(outlet) {
  mount(outlet, spinner());
  let data;
  try {
    data = await api.get('/servers/');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const { items, canManage } = data;
  const reload = () => renderConnections(outlet);
  const dbCount = items.reduce((n, i) => n + i.databases.length, 0);

  // 접기/펴기는 권한과 무관하게 보는 사람 모두에게 필요하다.
  // 서버가 여러 개일 때만 의미가 있으므로 그때만 내놓는다.
  const allOpen = items.every((i) => !collapsed.has(i.server.id));
  const foldBtn = items.length > 1
    ? h('button.btn', {
      type: 'button',
      onclick: () => {
        if (allOpen) for (const i of items) collapsed.add(i.server.id);
        else collapsed.clear();
        reload();
      },
    }, icon('list'), allOpen ? '모두 접기' : '모두 펼치기')
    : null;

  mount(outlet,
    pageHeader('DB 커넥션', `서버 ${items.length}개 · DB ${dbCount}개`, [
      foldBtn,
      canManage && items.length > 1
        ? h('button.btn', { type: 'button', onclick: () => openMergeDialog(items, reload) },
            icon('copy'), '서버 합치기')
        : null,
      canManage
        ? h('button.btn.btn-primary', { type: 'button', onclick: () => openServerForm(null, reload) },
            icon('plus'), '서버 등록')
        : null,
    ]),
    items.length === 0
      ? emptyState(canManage
          ? '등록된 서버가 없습니다. 서버를 등록하고 관리할 DB를 고르세요.'
          : '접근 가능한 DB가 없습니다.')
      : h('div.server-list', {}, items.map((item) => serverCard(item, canManage, reload))),
  );
}

// ---------- 서버 ----------

function serverCard(item, canManage, reload) {
  const srv = item.server;
  const isOpen = !collapsed.has(srv.id);
  const dbs = item.databases;

  const body = h('div.server-dbs', { style: { display: isOpen ? '' : 'none' } },
    dbs.length === 0
      ? h('p.muted.small', {}, '관리 중인 DB가 없습니다.')
      : dbs.map((d) => dbRow(d, srv, canManage, reload)),
  );

  const toggle = h('button.server-toggle', {
    type: 'button',
    'aria-expanded': String(isOpen),
    onclick: (e) => {
      const nowOpen = collapsed.has(srv.id);
      if (nowOpen) collapsed.delete(srv.id); else collapsed.add(srv.id);
      body.style.display = nowOpen ? '' : 'none';
      e.currentTarget.setAttribute('aria-expanded', String(nowOpen));
      e.currentTarget.classList.toggle('is-collapsed', !nowOpen);
    },
  }, icon('play', 12));
  toggle.classList.toggle('is-collapsed', !isOpen);

  return h('section.server-card', { class: srv.enabled ? '' : 'is-off' },
    h('header.server-head', {},
      toggle,
      h('div.server-title', {},
        dbLogo(srv.kind, 18),
        h('h2', {}, srv.name),
        badge(kindLabel(srv.kind), 'neutral'),
        srv.enabled ? null : badge('비활성', 'neutral'),
      ),
      h('div.server-addr', {},
        srv.kind === 'sqlite' ? '파일 기반' : `${srv.host}:${srv.port}`,
        srv.username ? ` · ${srv.username}` : '',
      ),
      serverStatus(dbs),
      h('div.server-actions', {},
        canManage ? h('button.btn.btn-small', {
          type: 'button', onclick: () => openAddDatabases(item, reload),
        }, icon('plus'), 'DB 추가') : null,
        canManage ? h('button.btn.btn-small', {
          type: 'button', onclick: () => openServerForm(srv, reload),
        }, icon('edit'), '서버 수정') : null,
        canManage ? h('button.icon-btn.danger', {
          type: 'button', title: '서버 삭제',
          onclick: () => deleteServer(srv, dbs, reload),
        }, icon('trash')) : null,
      ),
    ),
    srv.tags.length ? h('div.tag-row', {}, srv.tags.map((t) => badge(t, 'neutral'))) : null,
    srv.note ? h('p.conn-note', {}, srv.note) : null,
    body,
  );
}

// serverStatus는 소속 DB의 마지막 연결 결과를 한 줄로 요약한다.
// 서버 자체의 상태를 따로 저장하지 않는 이유: 접속은 언제나 특정 DB에 하는 것이고,
// 별도로 저장하면 두 값이 어긋나는 순간 어느 쪽을 믿을지 알 수 없다.
function serverStatus(dbs) {
  const failed = dbs.filter((d) => d.connection.lastCheckOk === false);
  const ok = dbs.filter((d) => d.connection.lastCheckOk === true);
  if (failed.length) {
    return h('span.server-status', {}, h('span.dot.dot-fail'), `${failed.length}개 실패`);
  }
  if (ok.length) {
    return h('span.server-status', {}, h('span.dot.dot-ok'), `${ok.length}개 정상`);
  }
  return h('span.server-status', {}, h('span.dot.dot-unknown'), '확인 전');
}

function dbRow(item, srv, canManage, reload) {
  const c = item.connection;
  const info = kindInfo(c.kind);
  const caps = info?.capabilities ?? {};

  const dot = c.lastCheckOk === null || c.lastCheckOk === undefined
    ? h('span.dot.dot-unknown', { title: '연결 테스트 이력 없음' })
    : c.lastCheckOk
      ? h('span.dot.dot-ok', { title: `정상 · ${relativeTime(c.lastCheckAt)}` })
      : h('span.dot.dot-fail', { title: `실패 · ${c.lastCheckMsg}` });

  const testBtn = h('button.btn.btn-small', { type: 'button' }, icon('play'), '테스트');
  testBtn.addEventListener('click', async () => {
    testBtn.disabled = true;
    testBtn.textContent = '테스트 중…';
    try {
      const res = await api.post(`/connections/${c.id}/test`);
      if (res.ok) {
        toast(`연결 성공 — ${res.server?.version ?? ''} (${res.server?.latencyMs?.toFixed(1) ?? '?'}ms)`, 'success');
      } else {
        toast(`연결 실패 — ${res.message}`, 'error', 8000);
      }
      reload();
    } catch (err) {
      toastError(err);
      testBtn.disabled = false;
    }
  });

  return h('div.db-row', { class: item.accessible ? '' : 'is-locked' },
    dot,
    h('div.db-main', {},
      h('div.db-title', {},
        h('span.db-name', {}, c.name),
        envBadge(c.environment),
        c.selfEnabled ? null : badge('비활성', 'neutral'),
      ),
      h('div.db-meta', {},
        h('code', {}, c.databaseName || '—'),
        ' · ',
        item.accessible ? levelBadge(item.level) : badge('접근 불가', 'neutral'),
        (item.caps ?? []).length
          ? h('span.db-caps', {}, (item.caps).map((cap) => badge(capLabel(cap), 'accent')))
          : null,
      ),
      c.lastCheckOk === false ? h('p.conn-error', {}, icon('alert'), c.lastCheckMsg) : null,
    ),
    h('div.db-actions', {},
      item.accessible ? testBtn : null,
      item.accessible && caps.explore
        ? h('a.btn.btn-small', { href: `/nosql?conn=${encodeURIComponent(c.id)}` }, icon('list'), '탐색')
        : null,
      canManage ? h('button.btn.btn-small', {
        type: 'button', onclick: () => openConnForm(c, srv, reload),
      }, icon('edit'), '수정') : null,
      canManage ? h('button.icon-btn.danger', {
        type: 'button', title: 'DB 삭제', onclick: () => deleteConn(c, reload),
      }, icon('trash')) : null,
    ),
  );
}

function capLabel(cap) {
  return state.meta?.capabilities?.find((x) => x.value === cap)?.label ?? cap;
}

// ---------- 서버 등록/수정 ----------

function openServerForm(existing, reload) {
  const isEdit = Boolean(existing);
  const kinds = state.meta?.dbKinds ?? [];

  const name = input({ value: existing?.name ?? '', placeholder: 'prod-main-mysql' });
  const kind = select(
    kinds.map((k) => ({ value: k.kind, label: k.label })),
    { value: existing?.kind ?? 'mysql' },
  );
  const environment = select(
    [{ value: 'dev', label: '개발' }, { value: 'prod', label: '운영' }],
    { value: existing?.defaultEnvironment ?? 'dev' },
  );
  const host = input({ value: existing?.host ?? '', placeholder: 'localhost' });
  const port = input({ type: 'number', value: existing?.port ?? '', placeholder: '3306' });
  const username = input({ value: existing?.username ?? '', autocomplete: 'off' });
  const password = input({
    type: 'password', autocomplete: 'new-password',
    placeholder: isEdit ? '변경하지 않으려면 비워두세요' : '',
  });
  const tags = input({ value: (existing?.tags ?? []).join(', '), placeholder: 'core, billing' });
  const note = textarea({ value: existing?.note ?? '', placeholder: '용도, 담당자 등' });
  const enabled = checkbox('활성화', { checked: existing?.enabled ?? true });
  // 첫 DB는 서버를 만들 때 함께 등록한다. 서버만 있고 DB가 없는 상태는
  // 화면에서 아무것도 할 수 없는 껍데기이기 때문이다.
  const firstDB = input({ placeholder: 'appdb' });

  const hostField = field('호스트', host);
  const portField = field('포트', port, '비워두면 기본 포트를 사용합니다');
  const firstDBField = field('첫 데이터베이스', firstDB,
    '등록 후 "DB 추가"로 같은 서버의 다른 DB를 더할 수 있습니다');
  const optionsBox = h('div.option-grid');
  const optionInputs = new Map();

  const syncKind = () => {
    const info = kinds.find((k) => k.kind === kind.value);
    const isFile = kind.value === 'sqlite';

    hostField.style.display = isFile ? 'none' : '';
    portField.style.display = isFile ? 'none' : '';
    firstDBField.querySelector('.field-label').textContent =
      isFile ? '파일 경로' : `첫 ${info?.dbLabel ?? '데이터베이스'}`;
    firstDB.placeholder = isFile ? './data/app.db' : 'appdb';
    if (!port.value && info?.defaultPort) port.placeholder = String(info.defaultPort);

    const previous = new Map([...optionInputs].map(([k, el]) => [k, el.value]));
    optionInputs.clear();
    optionsBox.replaceChildren();
    for (const hint of info?.optionHints ?? []) {
      const value = previous.get(hint.key) ?? existing?.options?.[hint.key] ?? '';
      const el = hint.choices?.length
        ? select([
          { value: '', label: '기본값' },
          ...hint.choices.map((c) => ({ value: c, label: c })),
          ...(value && !hint.choices.includes(value) ? [{ value, label: `${value} (알 수 없음)` }] : []),
        ], { value })
        : input({ value, placeholder: hint.placeholder ?? '' });
      optionInputs.set(hint.key, el);
      optionsBox.appendChild(field(hint.label, el, hint.help));
    }
  };
  kind.addEventListener('change', syncKind);
  syncKind();

  const serverPayload = () => {
    const options = {};
    for (const [key, el] of optionInputs) {
      const v = el.value.trim();
      if (v) options[key] = v;
    }
    const payload = {
      name: name.value.trim(),
      kind: kind.value,
      host: host.value.trim(),
      port: port.value ? Number(port.value) : 0,
      options,
      defaultEnvironment: environment.value,
      tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean),
      note: note.value.trim(),
      enabled: enabled.querySelector('input').checked,
      username: username.value.trim(),
    };
    if (password.value || !isEdit) payload.password = password.value;
    return payload;
  };

  const testResult = h('div.test-result');
  const testBtn = h('button.btn', { type: 'button' }, icon('play'), '연결 테스트');
  testBtn.addEventListener('click', async () => {
    testBtn.disabled = true;
    mount(testResult, h('span.muted', {}, '테스트 중…'));
    try {
      const body = { ...serverPayload(), environment: environment.value };
      body.databaseName = firstDB.value.trim();
      const qs = isEdit ? `?serverId=${encodeURIComponent(existing.id)}` : '';
      const res = await api.post(`/connections/test${qs}`,
        isEdit ? { ...body, serverId: existing.id } : body);
      mount(testResult, res.ok
        ? h('span.ok-text', {}, icon('check'),
            `연결 성공 — ${res.server?.version ?? ''} (${res.server?.latencyMs?.toFixed(1) ?? '?'}ms)`)
        : h('span.err-text', {}, icon('alert'), res.message));
    } catch (err) {
      mount(testResult, h('span.err-text', {}, icon('alert'), err.message));
    } finally {
      testBtn.disabled = false;
    }
  });

  const submit = async (close) => {
    try {
      if (isEdit) {
        const res = await api.put(`/servers/${existing.id}`, serverPayload());
        const n = res.server?.databaseCount ?? 0;
        toast(n > 1 ? `서버를 수정했습니다 — DB ${n}개에 반영됩니다` : '서버를 수정했습니다', 'success');
      } else {
        // 첫 DB와 함께 만든다. 커넥션 API가 서버까지 만들어 준다.
        await api.post('/connections/', {
          ...serverPayload(),
          environment: environment.value,
          databaseName: firstDB.value.trim(),
        });
        toast('서버와 첫 DB를 등록했습니다', 'success');
      }
      close();
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  openModal({
    title: isEdit ? `서버 수정 — ${existing.name}` : 'DB 서버 등록',
    width: 720,
    body: () => [
      isEdit && existing.databaseCount > 1
        ? h('p.notice.notice-warn', {}, icon('alert'),
            `이 서버의 DB ${existing.databaseCount}개가 아래 접속 정보를 함께 씁니다. 변경은 전부에 반영됩니다.`)
        : null,
      h('div.form-grid', {},
        field('서버 이름', name, '식별용 고유 이름'),
        field('기본 환경', environment, '이 서버에 DB를 추가할 때의 기본값입니다'),
        field('DB 종류', kind),
      ),
      h('div.form-grid', {}, hostField, portField),
      isEdit ? null : firstDBField,
      h('div.form-grid', {}, field('계정', username), field('비밀번호', password)),
      optionInputs.size
        ? h('details.form-section', { open: true }, h('summary', {}, '접속 옵션'), optionsBox)
        : optionsBox,
      h('div.form-grid', {},
        field('태그', tags, '콤마로 구분'),
        h('div.field', {}, h('span.field-label', {}, ' '), enabled)),
      field('메모', note),
      h('div.test-row', {}, testBtn, testResult),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', { type: 'button', onclick: () => submit(close) },
        isEdit ? '저장' : '등록'),
    ],
  });
}

// ---------- DB 일괄 추가 ----------

function openAddDatabases(item, reload) {
  const srv = item.server;
  const listBox = h('div.dblist');
  const manual = textarea({
    rows: 3,
    placeholder: srv.kind === 'redis' ? '0\n1' : 'appdb\nanalytics',
  });
  const environment = select(
    [{ value: 'dev', label: '개발' }, { value: 'prod', label: '운영' }],
    { value: srv.defaultEnvironment },
  );
  const tags = input({ placeholder: 'core, billing' });
  const boxes = new Map();

  const loadBtn = h('button.btn', { type: 'button' }, icon('refresh'), 'DB 목록 불러오기');
  loadBtn.addEventListener('click', load);

  async function load() {
    loadBtn.disabled = true;
    mount(listBox, spinner('서버에서 DB 목록을 읽는 중…'));
    try {
      const res = await api.get(`/servers/${srv.id}/databases`);
      boxes.clear();
      mount(listBox, res.databases.length === 0
        ? h('p.muted.small', {}, '가져올 DB가 없습니다.')
        : h('div.dblist-grid', {}, res.databases.map((db) => {
            const box = h('input', {
              type: 'checkbox',
              // 이미 등록된 것은 다시 고를 수 없다. 고를 수 있는데 실패하는 항목은
              // "왜 안 되지"를 만든다.
              disabled: db.registered,
              // 시스템 DB와 빈 Redis DB는 기본 선택에서 뺀다.
              checked: !db.registered && !db.system,
            });
            if (!db.registered) boxes.set(db.name, box);
            return h('label.dblist-item', { class: db.registered ? 'is-registered' : '' },
              box,
              h('span.dblist-name', {}, db.name),
              db.system ? badge('시스템', 'neutral') : null,
              db.registered ? badge('등록됨', 'info') : null,
              db.note ? h('span.dblist-note', {}, db.note) : null,
            );
          })));
    } catch (err) {
      mount(listBox,
        h('p.notice.notice-warn', {}, icon('alert'), err.message),
        h('p.field-help', {}, '아래에 이름을 직접 입력해 추가할 수 있습니다.'));
    } finally {
      loadBtn.disabled = false;
    }
  }

  // 목록을 읽을 수 있는 종류면 열자마자 읽는다 — 버튼을 한 번 더 누르게 할 이유가 없다.
  if (item.canListDatabases) setTimeout(load, 0);

  const submit = async (close, btn) => {
    const picked = [...boxes].filter(([, box]) => box.checked).map(([name]) => name);
    for (const line of manual.value.split(/[\n,]/)) {
      const v = line.trim();
      if (v && !picked.includes(v)) picked.push(v);
    }
    if (picked.length === 0) {
      toast('추가할 DB를 하나 이상 고르세요', 'error');
      return;
    }
    btn.disabled = true;
    try {
      const res = await api.post(`/servers/${srv.id}/databases`, {
        databases: picked,
        environment: environment.value,
        tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean),
      });
      close();
      // 부분 성공을 그대로 알린다. "몇 개 추가됨"만 보여주면 빠진 것을 알아채지 못한다.
      if (res.failed?.length) {
        toast(`${res.created.length}개 추가 · ${res.failed.length}개 실패 — ` +
          res.failed.map((f) => `${f.database}(${f.reason})`).join(', '), 'warn', 10000);
      } else {
        toast(`DB ${res.created.length}개를 추가했습니다`, 'success');
      }
      reload();
    } catch (err) {
      toastError(err);
      btn.disabled = false;
    }
  };

  openModal({
    title: `DB 추가 — ${srv.name}`,
    width: 640,
    body: () => [
      item.canListDatabases
        ? h('div.test-row', {}, loadBtn)
        : h('p.field-help', {},
            `${kindLabel(srv.kind)}는 목록을 읽어올 수 없어 이름을 직접 입력합니다.`),
      listBox,
      field('직접 입력', manual, '줄바꿈이나 콤마로 구분합니다'),
      h('div.form-grid', {}, field('환경', environment), field('태그', tags, '콤마로 구분')),
      h('p.field-help', {},
        '이름은 "', h('b', {}, srv.name), ' / DB이름"으로 지어집니다. 나중에 바꿀 수 있습니다.'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button', onclick: (e) => submit(close, e.currentTarget),
      }, '추가'),
    ],
  });
}

// ---------- 서버 병합 ----------

function openMergeDialog(items, reload) {
  const target = select(
    items.map((i) => ({ value: i.server.id, label: `${i.server.name} (${i.server.databaseCount}개)` })),
    { value: items[0].server.id },
  );
  const sourceBox = h('div.dblist');
  const dropEmpty = checkbox('비게 된 서버 삭제', { checked: true });

  const drawSources = () => {
    const chosen = items.find((i) => i.server.id === target.value);
    mount(sourceBox, h('div.dblist-grid', {},
      items
        .filter((i) => i.server.id !== target.value)
        .map((i) => {
          const same = i.server.kind === chosen.server.kind;
          return h('label.dblist-item', { class: same ? '' : 'is-registered' },
            h('input', { type: 'checkbox', disabled: !same, dataset: { id: i.server.id } }),
            h('span.dblist-name', {}, i.server.name),
            badge(kindLabel(i.server.kind), same ? 'neutral' : 'danger'),
            h('span.dblist-note', {},
              same ? `${i.server.host}:${i.server.port} · DB ${i.server.databaseCount}개` : '종류가 달라 합칠 수 없습니다'),
          );
        }),
    ));
  };
  target.addEventListener('change', drawSources);
  drawSources();

  openModal({
    title: '서버 합치기',
    width: 640,
    body: () => [
      h('p.field-help', {},
        '같은 서버를 여러 번 등록했다면 하나로 합칠 수 있습니다. ',
        h('b', {}, '옮겨진 DB는 대상 서버의 자격증명을 쓰게 됩니다'),
        ' — 두 서버의 계정이 다르면 접속이 끊깁니다. 합친 뒤 연결 테스트로 확인하세요.'),
      field('대상 서버', target, '이 서버의 접속 정보가 남습니다'),
      h('span.field-label', {}, '합칠 서버'),
      sourceBox,
      dropEmpty,
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          const ids = [...sourceBox.querySelectorAll('input:checked')].map((b) => b.dataset.id);
          if (ids.length === 0) {
            toast('합칠 서버를 고르세요', 'error');
            return;
          }
          e.currentTarget.disabled = true;
          try {
            const res = await api.post(`/servers/${target.value}/merge`, {
              sourceServerIds: ids,
              dropEmpty: dropEmpty.querySelector('input').checked,
            });
            close();
            toast(`DB ${res.moved}개를 옮겼습니다 (서버 ${res.droppedServers}개 정리)`, 'success');
            reload();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, '합치기'),
    ],
  });
}

// ---------- DB 수정 ----------

async function openConnForm(existing, srv, reload) {
  const shared = srv.databaseCount > 1;
  const name = input({ value: existing.name });
  // 담당 노드 고르개는 클러스터일 때만 나타난다. 단일 서버에서는 물어볼 것이 없고,
  // 빈 고르개 하나가 "내가 뭔가 설정해야 하나"라는 질문을 만든다.
  const nodes = await clusterNodes();
  const nodeSelect = nodes.length
    ? select([{ value: '', label: '(요청을 받은 노드에서 접속)' },
      ...nodes.map((n) => ({ value: n.id, label: `${n.name}${n.role === 'master' ? ' (마스터)' : ''}` }))],
    { value: existing.nodeId ?? '' })
    : null;
  const environment = select(
    [{ value: 'dev', label: '개발' }, { value: 'prod', label: '운영' }],
    { value: existing.environment },
  );
  const databaseName = input({ value: existing.databaseName });
  const tags = input({ value: (existing.tags ?? []).join(', '), placeholder: 'core, billing' });
  const note = textarea({ value: existing.note ?? '' });
  const enabled = checkbox('활성화', { checked: existing.selfEnabled });

  const submit = async (close) => {
    try {
      await api.put(`/connections/${existing.id}`, {
        name: name.value.trim(),
        environment: environment.value,
        databaseName: databaseName.value.trim(),
        tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean),
        note: note.value.trim(),
        enabled: enabled.querySelector('input').checked,
        nodeId: nodeSelect ? nodeSelect.value : (existing.nodeId ?? ''),
        // 접속 정보는 서버의 것이다. 여기서 보내지 않으면 서버는 손대지 않는다.
      });
      toast('DB를 수정했습니다', 'success');
      close();
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  openModal({
    title: `DB 수정 — ${existing.name}`,
    width: 560,
    body: () => [
      h('p.field-help', {},
        '접속 정보와 계정은 서버 ', h('b', {}, srv.name), '의 설정입니다',
        shared ? ` (DB ${srv.databaseCount}개가 함께 씁니다).` : '.',
        ' 바꾸려면 "서버 수정"을 여세요.'),
      field('이름', name),
      h('div.form-grid', {},
        field('환경', environment, '운영으로 지정하면 삭제·마이그레이션에 추가 확인이 요구됩니다'),
        field('데이터베이스', databaseName)),
      h('div.form-grid', {},
        field('태그', tags, '콤마로 구분'),
        h('div.field', {}, h('span.field-label', {}, ' '), enabled)),
      existing.serverEnabled ? null : h('p.notice.notice-warn', {}, icon('alert'),
        '서버가 비활성 상태라 이 DB는 켜 두어도 동작하지 않습니다.'),
      nodeSelect
        ? field('담당 노드', nodeSelect,
          '이 DB에 접속할 서버입니다. 사설망 안에 있어 특정 서버에서만 닿는 DB에 지정하세요. '
          + '조회·질의·데이터 수정·SQL 실행이 그 노드에서 실행됩니다. '
          + '지표 수집과 백업·마이그레이션은 마스터가 하므로 마스터도 그 DB에 닿아야 합니다.')
        : null,
      field('메모', note),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', { type: 'button', onclick: () => submit(close) }, '저장'),
    ],
  });
}

// ---------- 삭제 ----------

// 되돌릴 수 없는 것을 되돌릴 수 없다고만 적어 두면 아무도 읽지 않는다.
// 무엇이 몇 개 사라지는지 세어 보여 주면 그때야 손이 멈춘다.
const KEPT_METRIC_ONLY = new Set(['metric', 'event', 'snapshot', 'access']);

// impactDetails는 삭제 영향 목록을 대화상자에 넣을 노드로 만든다.
function impactDetails(items) {
  if (!items.length) return null;
  const lost = items.filter((i) => !i.kept);
  const kept = items.filter((i) => i.kept);
  return h('div.impact-box', {},
    lost.length
      ? h('div', {},
          h('p.impact-head', {}, icon('alert', 14), '함께 삭제됩니다'),
          h('ul.impact-list', {}, lost.map((i) =>
            h('li', {}, i.label, h('b', {}, `${i.count.toLocaleString('ko-KR')}개`)))))
      : null,
    kept.length
      ? h('div', {},
          h('p.impact-head.is-kept', {}, '기록은 남고 이 DB와의 연결만 끊깁니다'),
          h('ul.impact-list', {}, kept.map((i) =>
            h('li', {}, i.label, h('b', {}, `${i.count.toLocaleString('ko-KR')}개`)))))
      : null,
  );
}

// needsNameConfirm은 이름을 받아 적게 할지 정한다.
// 지표·이벤트처럼 다시 쌓이는 것만 사라진다면 굳이 타자를 치게 하지 않는다.
function needsNameConfirm(items) {
  return items.some((i) => !i.kept && !KEPT_METRIC_ONLY.has(i.key));
}

async function deleteConn(c, reload) {
  const isProd = c.environment === 'prod';
  let items = [];
  try {
    const res = await api.get(`/connections/${encodeURIComponent(c.id)}/impact`);
    items = res.items ?? [];
  } catch {
    // 영향 조회가 실패해도 삭제 자체는 막지 않는다. 대신 목록 없이 경고만 남는다.
    items = [];
  }

  const ok = await confirmDialog({
    title: 'DB 삭제',
    message: isProd
      ? `운영 DB "${c.name}" 을 삭제합니다. 실제 데이터베이스는 지워지지 않지만, 이 앱에 쌓인 아래 기록은 되돌릴 수 없습니다.`
      : `"${c.name}" 을 관리 목록에서 제거합니다. 실제 데이터베이스는 지워지지 않습니다.`,
    details: items.length
      ? impactDetails(items)
      : h('p.field-help', {}, '이 DB에 딸린 ERD·마이그레이션·이벤트 기록은 없습니다.'),
    confirmLabel: '삭제',
    danger: true,
    requireText: isProd || needsNameConfirm(items) ? c.name : null,
  });
  if (!ok) return;
  try {
    const qs = isProd ? `?confirm=${encodeURIComponent(c.name)}` : '';
    await api.del(`/connections/${c.id}${qs}`);
    toast('DB를 삭제했습니다', 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}

async function deleteServer(srv, dbs, reload) {
  // 서버 삭제는 그 아래 DB를 전부 지운다. 각 DB의 영향을 합쳐서 보여준다 —
  // 이 앱에서 한 번에 가장 많은 것이 사라지는 동작이다.
  let items = [];
  try {
    const results = await Promise.all((dbs ?? []).map((i) =>
      api.get(`/connections/${encodeURIComponent(i.connection.id)}/impact`)
        .then((r) => r.items ?? [])
        .catch(() => [])));
    const merged = new Map();
    for (const list of results) {
      for (const it of list) {
        const prev = merged.get(it.key);
        if (prev) prev.count += it.count;
        else merged.set(it.key, { ...it });
      }
    }
    items = [...merged.values()];
  } catch {
    items = [];
  }

  const ok = await confirmDialog({
    title: '서버 삭제',
    message: srv.databaseCount > 0
      ? `서버 "${srv.name}" 과 그 아래 DB ${srv.databaseCount}개를 삭제합니다. `
        + '실제 데이터베이스는 지워지지 않지만, 이 앱에 쌓인 기록은 되돌릴 수 없습니다.'
      : `서버 "${srv.name}" 을 삭제합니다.`,
    details: items.length ? impactDetails(items) : null,
    confirmLabel: '삭제',
    danger: true,
    // DB가 딸린 서버는 언제나 이름 확인을 요구한다. 운영/개발을 따지지 않는 이유는
    // 한 서버 아래에 둘이 섞여 있을 수 있고, 사라지는 양 자체가 크기 때문이다.
    requireText: srv.databaseCount > 0 ? srv.name : null,
  });
  if (!ok) return;
  try {
    const qs = srv.databaseCount > 0 ? `?confirm=${encodeURIComponent(srv.name)}` : '';
    await api.del(`/servers/${srv.id}${qs}`);
    toast('서버를 삭제했습니다', 'success');
    reload();
  } catch (err) {
    toastError(err);
  }
}


// clusterNodes는 담당 노드로 고를 수 있는 노드 목록이다.
//
// 실패를 삼키는 이유: 클러스터가 아니거나 이 사람에게 클러스터 조회 권한이 없으면
// 목록이 없을 뿐이고, 그 경우 DB 수정은 지금까지처럼 동작해야 한다.
async function clusterNodes() {
  try {
    const res = await api.get('/cluster/');
    if (!res?.status?.enabled) return [];
    return (res.nodes ?? []).filter((n) => n.status === 'active');
  } catch {
    return [];
  }
}

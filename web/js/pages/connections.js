// DB 서버와 그 아래 관리 대상 DB 화면.
//
// 서버로 묶어 보여주는 이유: 접속 정보와 자격증명은 서버의 것이고 그 아래 DB들이
// 함께 쓴다. 평평한 목록으로 두면 같은 호스트가 열 줄 반복되고, 비밀번호를 어디서
// 고쳐야 하는지가 보이지 않는다.
import { api } from '../core/api.js';
import { withProject, currentProjectID, hasProjects } from '../core/project.js';
import { state, kindInfo, kindLabel } from '../core/store.js';
import {
  h, mount, icon, field, input, select, textarea, checkbox, spinner, emptyState,
  pageHeader, openModal, confirmDialog, toast, toastError, badge, envBadge,
  levelBadge, relativeTime,
} from '../core/ui.js';
import { dbLogo } from '../core/dblogo.js';
import { errorPanel } from './users.js';
import { projectGuard } from './projects.js';

// 접힌 서버를 기억한다. 다시 그릴 때마다 전부 펴지면 방금 접은 것이 되살아난다.
const collapsed = new Set();

export async function renderConnections(outlet) {
  // 프로젝트가 없으면 DB를 등록할 곳도 없다. "커넥션이 없습니다"라고만 말하면
  // 없는 이유가 어디에도 드러나지 않는다.
  if (!hasProjects()) {
    mount(outlet, projectGuard('DB 커넥션'));
    return;
  }
  mount(outlet, spinner());
  let data;
  try {
    data = await api.get(withProject('/servers/'));
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
        // 담당 노드가 있으면 주소 옆에 붙인다. 이 서버의 DB들이 어디서 실행되는지가
        // 목록에서 바로 보여야 한다 — 안 보이면 "왜 이 DB만 안 되지"를 서버 설정까지
        // 올라가서야 알게 된다.
        srv.nodeId ? ` · 담당 ${nodeName(srv.nodeId)}` : '',
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
        // 서버와 다른 노드를 쓰는 DB에만 붙인다. 서버를 따르는 DB에까지 붙이면
        // 모든 줄에 배지가 생겨 아무것도 구분해 주지 못한다.
        c.nodeId && c.nodeId !== srv.nodeId
          ? badge(`담당 ${nodeName(c.nodeId)} (예외)`, 'warn')
          : null,
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

async function openServerForm(existing, reload) {
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

  // 담당 노드.
  //
  // ── 왜 서버에 있는가 ────────────────────────────────────────────
  // "이 DB가 어느 사설망 안에 있는가"는 호스트·포트와 같은 급의 접속 사실이다. 그
  // 값이 DB마다 따로 있으면 서버 하나에 DB 다섯 개일 때 다섯 번 입력해야 하고,
  // 하나만 빠뜨리면 그 DB만 조용히 실패한다.
  //
  // 여기서 고른 값은 소속 DB가 **따라간다.** DB 수정 화면에 "서버를 따름"이 기본으로
  // 있고, 거기서 따로 고른 DB만 예외가 된다.
  const nodes = await clusterNodes();
  const nodeSelect = nodes.length
    ? select([{ value: '', label: '(요청을 받은 노드에서 접속)' },
      ...nodes.map((n) => ({ value: n.id, label: `${n.name}${n.role === 'master' ? ' (마스터)' : ''}` }))],
    { value: existing?.nodeId ?? '' })
    : null;
  const nodeImpact = h('p.field-help');

  // 담당 노드를 바꾸면 무엇이 함께 움직이는지 미리 보여준다.
  //
  // ── 왜 물어보는가 ───────────────────────────────────────────────
  // 이 설정 하나로 **그 서버의 DB 전부**의 접속 경로가 바뀐다. 숫자 없이 저장하면
  // 사람은 예전 감각(DB 하나 바꾸는 일)으로 누른다. 그리고 그 노드에 실제로 닿는지도
  // 여기서 확인한다 — 저장한 뒤에 알면 그 서버의 요청이 전부 502로 떨어진 것을
  // 한참 뒤에 발견한다.
  const refreshImpact = async () => {
    if (!isEdit || !nodeSelect) return;
    const want = nodeSelect.value;
    try {
      const res = await api.get(`/servers/${encodeURIComponent(existing.id)}/node-impact`
        + `?nodeId=${encodeURIComponent(want)}`);
      const parts = [res.message];
      if (res.reach?.ok === true) parts.push(`· "${res.reach.node}" 에서 접속 확인됨`);
      if (res.reach?.ok === false) parts.push(`· "${res.reach.node}" 에서 접속 실패: ${res.reach.message}`);
      mount(nodeImpact, parts.filter(Boolean).join(' '));
    } catch {
      // 미리보기를 못 받아도 저장은 되어야 한다. 여기서 막으면 확인 창이
      // 없어진 것이 아니라 저장 자체가 막힌 것이 된다.
      mount(nodeImpact, '');
    }
  };
  if (nodeSelect) nodeSelect.addEventListener('change', refreshImpact);
  if (isEdit) setTimeout(refreshImpact, 0);

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
      // 담당 노드는 서버의 접속 사실이다. 소속 DB가 이 값을 따른다.
      // 고르개가 없으면(단일 서버) 보내지 않는다 — 빈 값으로 덮으면 안 된다.
      nodeId: nodeSelect ? nodeSelect.value : (existing?.nodeId ?? ''),
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
            `연결 성공 — ${res.server?.version ?? ''} (${res.server?.latencyMs?.toFixed(1) ?? '?'}ms)`
            // 담당 노드가 시험한 경우 그 사실을 적는다. "테스트는 되는데 등록하니 안 된다"를
            // 겪은 사람이 가장 먼저 확인해야 하는 것이 어느 노드가 시험했는가다.
            + (res.testedBy ? ` · ${res.testedBy} 에서 확인` : ''))
        : h('span.err-text', {}, icon('alert'), res.message
            + (res.testedBy ? ` (${res.testedBy} 에서 확인)` : '')));
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
          // 보고 있는 프로젝트에 넣는다. 어디에 넣을지 다시 묻지 않는 이유:
          // 사이드바가 이미 그 답을 말하고 있고, 여기서 또 고르게 하면 두 답이
          // 갈라지는 자리가 생긴다. 다른 곳에 넣으려면 프로젝트를 먼저 옮긴다.
          projectId: currentProjectID(),
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
      nodeSelect
        ? field('담당 노드', nodeSelect,
          '이 서버에 접속할 노드입니다. 사설망 안에 있어 특정 서버에서만 닿는 DB에 지정하세요. '
          + '이 서버의 DB들이 이 값을 따릅니다 — DB 하나만 다르면 DB 수정에서 따로 고르세요.')
        : null,
      nodeSelect ? nodeImpact : null,
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
    // 어느 노드가 읽는지 버튼 옆에 적는다. 담당 노드가 있는 서버는 **그 노드가**
    // 접속해 목록을 읽는다 — 마스터가 닿지 못하는 사설망 DB에서 이 한 줄이
    // "왜 이건 되지"의 답이 된다.
    if (srv.nodeId) loadBtn.title = `담당 노드 "${nodeName(srv.nodeId)}" 에서 읽습니다`;
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
        // 프로젝트는 보내지 않는다. 서버가 이미 한 프로젝트의 것이고, DB는 그
        // 서버를 따라간다 — 근거는 하나여야 한다.
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
          // currentTarget 은 이벤트가 끝나면 null 이 된다. await 뒤에서 다시 만지면
          // 그 자리에서 예외가 나고, 그러면 실패를 알리는 토스트조차 뜨지 않는다 —
          // 눌렀는데 아무 일도 일어나지 않은 것처럼 보인다.
          const pressed = e.currentTarget;
          pressed.disabled = true;
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
            pressed.disabled = false;
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
  //
  // ── 기본값은 "서버를 따름"이다 ──────────────────────────────────
  // 담당 노드는 서버의 접속 사실이고, 이 DB에 담긴 값은 그것을 덮는 **예외**다.
  // 그래서 (1) 서버를 따르는 것이 기본이고 (2) 예외인 DB는 그 사실이 드러나야 한다 —
  // 드러나지 않으면 서버의 담당 노드를 바꿔도 안 따라가는 이유를 아무도 모른다.
  const nodes = await clusterNodes();
  const inherited = srv.nodeId ?? '';
  const inheritedName = nodes.find((n) => n.id === inherited)?.name ?? inherited;
  const nodeOptions = [{ value: '', label: '(요청을 받은 노드에서 접속)' }];
  if (inherited) {
    nodeOptions.unshift({ value: inherited, label: `(서버를 따름) ${inheritedName}` });
  }
  nodeOptions.push({ value: '__other__', label: '다른 노드를 직접 지정…' });
  nodeOptions.push(...nodes
    .filter((n) => n.id !== inherited)
    .map((n) => ({ value: n.id, label: `${n.name}${n.role === 'master' ? ' (마스터)' : ''}` })));

  // 저장된 값이 서버 값과 같으면 "서버를 따름"으로 보여준다. DB에 값이 남아 있어도
  // 실효 담당 노드는 같으므로, 여기서 구분해 보여주면 같은 결과가 두 가지로 보인다.
  const stored = existing.nodeId ?? '';
  const nodeValue = stored === inherited ? inherited : stored;
  const nodeSelect = nodes.length ? select(nodeOptions, { value: nodeValue }) : null;
  const nodeNote = h('p.field-help');
  const syncNodeNote = () => {
    if (!nodeSelect) return;
    if (nodeSelect.value === inherited) {
      mount(nodeNote, srv.nodeId
        ? '' // 서버를 따르고 있고 서버에 값이 있으면 설명할 것이 없다
        : '이 서버에는 담당 노드가 없습니다. 서버 수정에서 지정하면 이 DB가 따라갑니다.');
      return;
    }
    if (nodeSelect.value === '') {
      mount(nodeNote, '서버를 따르지 않고 요청을 받은 노드가 접속합니다.'
        + (inherited ? ` (서버는 "${inheritedName}" 로 지정되어 있습니다)` : ''));
      return;
    }
    mount(nodeNote, '서버와 다른 노드를 이 DB만 따로 씁니다(예외).');
  };
  if (nodeSelect) nodeSelect.addEventListener('change', syncNodeNote);
  syncNodeNote();
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
      // 고르개가 '__other__'(다른 노드를 직접 지정…)에 머물러 있으면 저장하지 않는다.
      // 그 값은 목록을 펼치는 자리일 뿐 실제 노드가 아니다 — 그대로 보내면 서버가
      // "그런 노드가 없다"로 거절하고, 사람은 목록에서 고르라는 말을 듣지 못한다.
      let picked = nodeSelect ? nodeSelect.value : (existing.nodeId ?? '');
      if (picked === '__other__') {
        toast('담당 노드를 목록에서 고르세요', 'error');
        return;
      }
      // 서버를 따르는 경우에는 **빈 값을 보낸다.** 저장된 값을 그대로 되돌려 보내면
      // 그 DB가 예외로 굳어 버리고, 나중에 서버의 담당 노드를 바꿔도 따라가지 않는다.
      if (picked === inherited) picked = '';
      await api.put(`/connections/${existing.id}`, {
        name: name.value.trim(),
        environment: environment.value,
        databaseName: databaseName.value.trim(),
        tags: tags.value.split(',').map((t) => t.trim()).filter(Boolean),
        note: note.value.trim(),
        enabled: enabled.querySelector('input').checked,
        nodeId: picked,
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
      nodeSelect ? nodeNote : null,
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


// nodeName은 노드 ID를 화면에 보일 이름으로 바꾼다.
//
// 캐시에 없으면 ID를 그대로 보여준다. 이름을 못 찾았다고 빈 칸을 남기면 "담당이
// 없다"로 읽히는데, 담당이 있는 것과 이름을 모르는 것은 전혀 다르다.
function nodeName(id) {
  if (!id) return '';
  return nodeCache.nodes.find((n) => n.id === id)?.name ?? id;
}

// clusterNodes는 담당 노드로 고를 수 있는 노드 목록이다.
//
// ── 왜 캐시하는가 ──────────────────────────────────────────────────
// 이 함수는 서버 수정 창과 DB 수정 창이 열릴 때마다, 그리고 목록의 DB 줄마다 불린다.
// 담당 노드가 서버 등급이 되면서 부르는 자리가 크게 늘었는데, 노드 목록은 몇 분에
// 한 번 바뀌는 값이다 — 창 하나 열 때마다 클러스터 API를 부르면 그만큼 화면이 늦게 뜬다.
//
// 캐시가 틀렸을 때의 결과는 "새로 뜬 노드가 목록에 없다"이지, 잘못된 저장이 아니다
// (저장 시 서버가 노드 존재를 다시 본다). 그래서 짧게(30초)만 들고 있는다.
let nodeCache = { at: 0, nodes: [] };
const NODE_CACHE_MS = 30_000;

// 실패를 삼키는 이유: 클러스터가 아니거나 이 사람에게 클러스터 조회 권한이 없으면
// 목록이 없을 뿐이고, 그 경우 DB 수정은 지금까지처럼 동작해야 한다.
async function clusterNodes() {
  if (Date.now() - nodeCache.at < NODE_CACHE_MS) return nodeCache.nodes;
  try {
    const res = await api.get('/cluster/');
    if (!res?.status?.enabled) {
      nodeCache = { at: Date.now(), nodes: [] };
      return [];
    }
    const nodes = (res.nodes ?? []).filter((n) => n.status === 'active');
    nodeCache = { at: Date.now(), nodes };
    return nodes;
  } catch {
    return [];
  }
}

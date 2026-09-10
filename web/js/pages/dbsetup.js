// DB 컨테이너 — 도커로 데이터베이스를 세우고 다루는 화면.
//
// ── 왜 상태를 먼저 보는가 ───────────────────────────────────────────
// 이 기능은 배치를 손봐야 켜지는 종류다. 소켓을 마운트했는가, 그 소켓을 열
// 권한이 있는가(이 앱은 nonroot 로 돈다), 데몬이 우리가 말하는 API 버전을
// 받는가. 셋 중 하나만 어긋나도 아무것도 되지 않는데, 그 사실이 "만들기가
// 실패했다"로만 나타나면 어디를 고쳐야 하는지 알 수 없다.
//
// 그래서 못 쓸 때 **무엇을 고쳐야 하는지**를 화면이 직접 말한다. 이 안내가
// 기능의 절반이다 — 서버에 붙어 로그를 뒤질 사람만 쓸 수 있는 기능이라면
// 화면에 둘 이유가 없다. 쓸 수 있을 때는 접어 둔다: 잘 되고 있는 것을 설명하는
// 표가 화면의 절반을 차지하면 정작 할 일이 아래로 밀린다.
//
// ── 왜 되풀어 읽는가 ────────────────────────────────────────────────
// 만들기는 배경에서 돈다(이미지 하나가 300MB 를 넘는다). 서버는 진행 상황을
// 인스턴스의 한 칸에 덮어쓰므로, 화면은 만드는 중인 것이 있을 때만 그 칸을
// 되풀어 읽는다. 스트리밍하지 않는 이유도 같다 — 이 작업이 만드는 정보는
// "지금 어디까지 왔는가" 하나뿐이라 덮어쓰면 된다.
import { h, mount, icon } from '../core/dom.js';
import { api } from '../core/api.js';
import {
  pageHeader, spinner, badge, emptyState, field, input, select, checkbox,
  openModal, confirmDialog, toast, toastError, relativeTime,
} from '../core/ui.js';
import { state } from '../core/store.js';
import { setScreenDetail } from '../core/screen.js';
import { currentProjectID, withProject, hasProjects } from '../core/project.js';
import { projectGuard } from './projects.js';

// 되풀어 읽는 간격. 1초로 두면 만드는 동안 메타 DB 가 이 폴링으로만 바쁘고,
// 5초로 두면 진행 문구가 뜸해서 멈춘 것처럼 보인다.
const POLL_MS = 2000;

// 상태 이름표. 표와 상세 창이 같은 것을 같은 말로 불러야 한다.
const STATUS = {
  creating: ['만드는 중', 'info'],
  running: ['도는 중', 'success'],
  stopped: ['멈춤', 'neutral'],
  unhealthy: ['이상 있음', 'warn'],
  failed: ['실패', 'danger'],
  removed: ['지움', 'neutral'],
};

let timer = null;

export async function renderDBSetup(root) {
  stopPolling();
  if (!hasProjects()) {
    mount(root, projectGuard('DB 컨테이너'));
    return;
  }
  mount(root, spinner('도커 상태를 확인하는 중…'));

  let data = null;
  let failure = null;
  try {
    data = await api.get('/docker/status');
  } catch (err) {
    failure = err;
  }

  const usable = Boolean(data?.status?.reachable && data.status.apiOk);
  let catalog = null;
  let instances = [];
  if (usable) {
    // 둘을 함께 읽는다. 목록이 먼저 오고 카탈로그가 뒤에 오면 화면이 두 번
    // 그려지고, 그 사이에 누른 버튼은 사라진 요소를 가리킨다.
    try {
      const [cat, list] = await Promise.all([
        api.get('/docker/catalog'),
        api.get(withProject('/docker/instances')),
      ]);
      catalog = cat;
      instances = list.items ?? [];
    } catch (err) {
      failure = err;
    }
  }
  draw(root, { data, failure, catalog, instances });
}

function draw(root, view) {
  const { data, failure, catalog, instances } = view;
  const st = data?.status ?? null;
  const usable = Boolean(st?.reachable && st.apiOk);
  setScreenDetail([
    st?.reachable ? `도커 ${st.version} (${st.os}/${st.architecture})` : '도커에 닿지 못함',
    usable ? `DB ${instances.length}개` : null,
  ].filter(Boolean));

  mount(root,
    pageHeader('DB 컨테이너', '도커로 데이터베이스를 세우고 실행·중단합니다',
      h('button.btn.btn-small', {
        type: 'button', onclick: () => renderDBSetup(root),
      }, icon('refresh'), '다시 확인')),
    failure && !usable ? gateCard(failure) : null,
    failure && usable
      ? h('div.notice.notice-danger', {}, icon('alert'),
        h('span', {}, failure.message ?? '목록을 읽지 못했습니다'))
      : null,
    usable ? statusStrip(st) : (failure ? null : statusCard(st, data?.apiVersion)),
    usable ? instancesCard(root, instances) : null,
    usable && catalog ? catalogCard(root, catalog) : null,
  );

  if (usable && instances.some((in_) => in_.status === 'creating')) {
    timer = setTimeout(() => renderDBSetup(root), POLL_MS);
  }
}

function stopPolling() {
  if (timer) clearTimeout(timer);
  timer = null;
}

// ---------- 만들기 ----------

// catalogCard는 고를 수 있는 DB 들이다.
function catalogCard(root, catalog) {
  const recipes = catalog.recipes ?? [];
  const unsupported = Object.entries(catalog.unsupported ?? {});
  return h('div.card', {},
    h('h2.card-title', {}, icon('plus'), '새 DB 만들기'),
    h('div.nosql-grid', {},
      ...recipes.map((r) => h('button.dbsetup-tile', {
        type: 'button', onclick: () => openCreateModal(root, r),
      },
      h('span.dbsetup-tile-name', {}, r.label),
      h('span.dbsetup-tile-blurb', {}, r.blurb),
      h('span.dbsetup-tile-meta', {}, `${r.versions[0]} · 포트 ${r.port}`)))),
    unsupported.length
      ? h('details.field-help', {},
        h('summary', {}, '여기 없는 것들'),
        h('dl.kv', {}, ...unsupported.flatMap(([kind, why]) => [
          h('dt', {}, kind), h('dd', {}, why),
        ])))
      : null);
}

// openCreateModal은 설정 칸을 그린다.
//
// 꼭 필요한 것만 위에 두고 나머지는 접는다. 처음 만드는 사람에게 스무 개의
// 칸을 한 번에 보여 주면 어느 것이 꼭 필요한지 알 수 없다 — 접는 기준은
// 서버의 레시피가 정한다(Field.Advanced).
function openCreateModal(root, recipe) {
  const values = {};
  for (const f of recipe.fields) {
    if (f.default !== undefined && f.default !== '') values[f.key] = f.default;
  }

  const nameInput = input({
    value: '', placeholder: `${recipe.id}1`, autofocus: true,
    oninput: (e) => { values.__name = e.target.value; },
  });
  const versionSel = select(recipe.versions.map((v) => ({ value: v, label: v })),
    { value: recipe.versions[0], onchange: (e) => { values.__version = e.target.value; } });
  values.__version = recipe.versions[0];

  const basic = recipe.fields.filter((f) => !f.advanced && !f.hidden);
  const advanced = recipe.fields.filter((f) => f.advanced && !f.hidden);

  const registerBox = checkbox('DB 커넥션으로 바로 등록', { checked: true });
  const body = h('div', {},
    h('p.field-help', {}, recipe.blurb),
    field('이름', nameInput,
      '컨테이너와 볼륨의 이름이 됩니다. 소문자로 시작하고 소문자·숫자·붙임표만 쓸 수 있습니다'),
    field('버전', versionSel, '태그를 고릅니다. 뒤에 바꾸려면 다시 만들어야 합니다'),
    ...basic.map((f) => fieldFor(f, values)),
    advanced.length
      ? h('details.dbsetup-advanced', {},
        h('summary', {}, '자세한 설정'),
        ...advanced.map((f) => fieldFor(f, values)))
      : null,
    h('div.dbsetup-register', {}, registerBox),
    (recipe.notes ?? []).length
      ? h('div.notice.notice-info', {}, icon('alert'),
        h('div', {}, ...recipe.notes.map((n) => h('p', {}, n))))
      : null);

  const submit = h('button.btn.btn-primary', { type: 'button' },
    icon('plus'), '만들기');
  const close = openModal({
    title: `${recipe.label} 만들기`, body, width: 620,
    footer: (closeFn) => [
      h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
      submit,
    ],
  });

  submit.onclick = async () => {
    const name = (values.__name ?? '').trim();
    const payload = {
      projectId: currentProjectID(),
      kind: recipe.id,
      name,
      version: values.__version,
      values: fieldValues(recipe, values),
      register: registerBox.querySelector('input').checked,
    };

    // 만들기 전에 계획을 받아 경고를 보여 준다. 이 만들기는 되돌리는 값이
    // 크다 — 컨테이너 하나와 볼륨 하나가 생기고, 잘못이면 지우는 것까지 해야
    // 한다. 값이 잘못되었으면 여기서 걸려서 아무것도 만들어지지 않는다.
    let plan;
    try {
      submit.disabled = true;
      plan = await api.post('/docker/plan', payload);
    } catch (err) {
      submit.disabled = false;
      toastError(err);
      return;
    }
    submit.disabled = false;

    const warnings = plan.plan?.warnings ?? [];
    if (warnings.length) {
      const ok = await confirmDialog({
        title: '이대로 만들까요?',
        message: warnings.join('\n'),
        confirmLabel: '만들기',
      });
      if (!ok) return;
    }

    try {
      await api.post('/docker/instances', payload);
      close();
      toast(`${name} 을(를) 만들고 있습니다`, 'info');
      renderDBSetup(root);
    } catch (err) {
      toastError(err);
    }
  };
}

// fieldFor는 서버가 준 필드 하나를 칸으로 그린다.
//
// 종류별 분기를 여기 한 곳에 두는 이유: 화면이 DB 마다 칸을 적어 두면 새 DB 를
// 더할 때 화면도 고쳐야 하고, 그때 한쪽만 고치면 값이 조용히 빠진다.
function fieldFor(f, values) {
  const set = (v) => { values[f.key] = v; };
  let control;
  switch (f.kind) {
    case 'bool': {
      const box = checkbox(f.label, { checked: f.default === 'true' });
      box.querySelector('input').onchange = (e) => set(e.target.checked ? 'true' : 'false');
      set(f.default === 'true' ? 'true' : 'false');
      return h('div.dbsetup-bool', {}, box,
        f.help ? h('span.field-help', {}, f.help) : null);
    }
    case 'select':
      control = select((f.choices ?? []).map((v) => ({ value: v, label: v })),
        { value: f.default ?? '', onchange: (e) => set(e.target.value) });
      break;
    case 'password':
      control = input({
        type: 'password', placeholder: f.placeholder ?? '',
        autocomplete: 'new-password', oninput: (e) => set(e.target.value),
      });
      break;
    case 'number':
      control = input({
        type: 'number', value: f.default ?? '', placeholder: f.placeholder ?? '',
        min: f.min ?? undefined, max: f.max ?? undefined,
        oninput: (e) => set(e.target.value),
      });
      break;
    default:
      control = input({
        value: f.default ?? '', placeholder: f.placeholder ?? '',
        oninput: (e) => set(e.target.value),
      });
  }
  return field(f.required ? `${f.label} *` : f.label, control, f.help);
}

// fieldValues는 서버에 보낼 값만 고른다.
//
// 빈 값은 보내지 않는다. 서버는 빈 값을 "정하지 않았다"로 읽어 이미지 기본값을
// 쓰는데, 빈 문자열을 보내면 그것이 곧 설정값이 된다 — 숫자 칸에서 그러면
// 컨테이너가 뜨지 않는다.
function fieldValues(recipe, values) {
  const out = {};
  for (const f of recipe.fields) {
    const v = values[f.key];
    if (v === undefined || v === null || v === '') continue;
    out[f.key] = String(v);
  }
  return out;
}

// ---------- 목록 ----------

function instancesCard(root, instances) {
  if (!instances.length) {
    return h('div.card', {},
      h('h2.card-title', {}, icon('database'), '만든 DB'),
      emptyState('아직 만든 DB 가 없습니다. 아래에서 종류를 고르세요'));
  }
  return h('div.card', {},
    h('h2.card-title', {}, icon('database'), '만든 DB',
      badge(`${instances.length}개`, 'neutral')),
    h('div.table-wrap', {},
      h('table.table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '이름'), h('th', {}, '종류'), h('th', {}, '상태'),
          h('th', {}, '주소'), h('th', {}, '만든 때'))),
        h('tbody', {}, ...instances.map((in_) => instanceRow(root, in_))))));
}

function instanceRow(root, in_) {
  const cells = [
    h('td', {}, h('div.dbsetup-name', {},
      h('strong', {}, in_.name),
      h('span.field-help', {}, in_.containerName))),
    h('td', {}, `${in_.kind} ${in_.version}`),
    h('td', {}, statusCell(in_)),
    h('td', {}, in_.hostPort > 0 ? h('code', {}, `:${in_.hostPort}`) : '—'),
    h('td', {}, relativeTime(in_.createdAt)),
  ];
  const row = h('tr', {
    class: in_.status === 'removed' ? 'row-muted' : undefined,
    onclick: () => openDetail(root, in_),
  }, ...cells);
  return row;
}

// statusCell은 상태 한 칸이다.
//
// 만드는 중에는 진행 문구를 함께 보여 준다. "만드는 중"만 오래 떠 있으면
// 멈춘 것과 구별되지 않고, 사람은 새로 고치기를 누르거나 다시 만든다.
function statusCell(in_) {
  const [label, kind] = STATUS[in_.status] ?? [in_.status, 'neutral'];
  return h('div.dbsetup-status', {},
    badge(label, kind),
    in_.status === 'creating' && in_.progress
      ? h('span.field-help', {}, in_.progress) : null,
    in_.status === 'failed' && in_.error
      ? h('span.field-help.dbsetup-error', {}, in_.error) : null);
}

// openDetail은 하나를 자세히 보고 다룬다.
//
// 실행·중단·다시 시작·지우기와 **로그**가 여기 모인다. 로그를 같은 창에 두는
// 이유: 이 버튼들을 누르는 이유의 대부분이 로그에 적혀 있다. 창을 갈라 두면
// "다시 시작했는데 또 죽는다"를 볼 때마다 두 창을 오가게 된다.
async function openDetail(root, summary) {
  const body = h('div.dbsetup-detail');
  const foot = h('div.dbsetup-actions');
  let stream = null;

  const stop = () => { stream?.close(); stream = null; };
  const close = openModal({
    title: summary.name, width: 720, body, footer: () => foot,
    // 창을 닫으면 스트림도 닫는다. 두지 않으면 창을 여닫을 때마다 도커 쪽에
    // 아무도 보지 않는 로그 스트림이 하나씩 쌓인다.
    onClose: stop,
  });

  const reload = async () => {
    let detail;
    try {
      detail = await api.get(`/docker/instances/${encodeURIComponent(summary.id)}`);
    } catch (err) {
      mount(body, h('div.notice.notice-danger', {}, icon('alert'),
        h('span', {}, err.message ?? '읽지 못했습니다')));
      return;
    }
    draw(detail);
  };

  const act = async (label, run) => {
    for (const b of foot.querySelectorAll('button')) b.disabled = true;
    try {
      await run();
      toast(`${summary.name} — ${label}`, 'success');
    } catch (err) {
      toastError(err);
    }
    // 무엇을 눌렀든 목록과 창을 다시 읽는다. 상태는 도커가 들고 있고, 우리가
    // 아는 것은 방금 부탁한 것뿐이다.
    stop();
    await reload();
    renderDBSetup(root);
  };

  function draw(detail) {
    const in_ = detail.instance;
    const recipe = detail.recipe;
    const label = (key) => recipe?.fields?.find((f) => f.key === key)?.label ?? key;
    const running = in_.status === 'running' || in_.status === 'unhealthy';
    const gone = in_.status === 'removed';

    const rows = [
      ['상태', (STATUS[in_.status] ?? [in_.status])[0]],
      ['컨테이너', in_.containerName],
      ['이미지', in_.image],
      ['볼륨', in_.volumeName || '없음 (데이터를 남기지 않습니다)'],
      ['포트', in_.hostPort > 0 ? `${in_.hostPort} → ${in_.port}` : `아직 없음 (안쪽 ${in_.port})`],
      ['커넥션', in_.connectionId ? '등록되어 있습니다' : '등록되지 않았습니다'],
      ['만든 사람', in_.createdByName || '—'],
    ];

    mount(body,
      in_.error
        ? h('div.notice.notice-danger', {}, icon('alert'), h('span', {}, in_.error))
        : null,
      h('dl.kv', {}, ...rows.flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, String(v))])),
      h('details.dbsetup-values', {},
        h('summary', {}, '만들 때 정한 값'),
        Object.keys(in_.values ?? {}).length
          ? h('dl.kv', {}, ...Object.entries(in_.values).flatMap(([k, v]) => [
            h('dt', {}, label(k)), h('dd', {}, String(v)),
          ]))
          : h('p.field-help', {}, '기본값으로 만들었습니다'),
        // 비밀번호는 보여 주지 않는다. 목록에도 실려 나오지 않는 값이고,
        // 여기서 한 번 보여 주면 그 화면을 캡처한 것이 곧 비밀이 아니게 된다.
        h('p.field-help', {}, '비밀번호는 저장되어 있지만 화면에는 보여주지 않습니다. '
          + '커넥션으로 등록했다면 접속에는 그 값이 쓰입니다.')),
      in_.connectionId
        ? h('a.btn.btn-small', { href: `/connections/${in_.connectionId}` },
          icon('link'), '커넥션 보기')
        : null,
      logSection(in_));

    mount(foot,
      running
        ? h('button.btn', {
          type: 'button',
          onclick: () => act('중단했습니다', () => api.post(`/docker/instances/${in_.id}/stop`)),
        }, icon('stop'), '중단')
        : null,
      !running && !gone
        ? h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => act('시작했습니다', () => api.post(`/docker/instances/${in_.id}/start`)),
        }, icon('play'), '시작')
        : null,
      running
        ? h('button.btn', {
          type: 'button',
          onclick: () => act('다시 시작했습니다', () => api.post(`/docker/instances/${in_.id}/restart`)),
        }, icon('refresh'), '다시 시작')
        : null,
      h('button.btn.btn-danger', {
        type: 'button', onclick: () => removeInstance(in_, act),
      }, icon('trash'), gone && !in_.containerId ? '기록 지우기' : '지우기'),
      h('span.dbsetup-actions-gap'),
      h('button.btn', { type: 'button', onclick: close }, '닫기'));
  }

  // logSection은 로그 상자다. 스트림은 여기서 붙인다.
  function logSection(in_) {
    if (!in_.containerId) {
      return h('div', {},
        h('h3.erd-sub', {}, '로그'),
        h('p.field-help', {}, '컨테이너가 없어 로그를 읽을 수 없습니다.'));
    }
    const note = h('span.field-help', {}, '따라가는 중…');
    const head = h('div.dbsetup-log-head', {}, h('h3.erd-sub', {}, '로그'), note);
    const box = h('div.dbsetup-log');

    stop();
    stream = new EventSource(`/api/v1/docker/instances/${in_.id}/logs?tail=200`);
    stream.addEventListener('log', (e) => appendLine(box, JSON.parse(e.data)));
    stream.addEventListener('end', () => {
      note.textContent = '로그가 끝났습니다 (컨테이너가 멈춰 있습니다)';
      stop();
    });
    stream.addEventListener('error', () => {
      // 끊기면 조용히 닫는다. 여기서 다시 붙으면 멈춘 컨테이너에 계속 매달린다 —
      // 창을 다시 열면 처음부터 받는다.
      note.textContent = '연결이 끊겼습니다 (창을 다시 열면 이어집니다)';
      stop();
    });
    return h('div', {}, head, box);
  }

  await reload();
}

// appendLine은 로그 한 줄을 붙인다.
//
// 맨 아래에 붙어 있을 때만 따라 내린다. 위로 올려 읽고 있는 사람을 끌어내리면
// 새 줄이 올 때마다 읽던 자리를 잃는다.
function appendLine(box, line) {
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  // stderr 를 빨갛게 칠하지 않는다. DB 는 평범한 시작 로그를 stderr 로 내는
  // 것이 흔해서(ClickHouse·PostgreSQL), 그러면 잘 뜬 DB 가 온통 오류로 보인다.
  // 어느 쪽에서 왔는지는 흐린 색으로만 구분한다.
  box.appendChild(h('div.dbsetup-log-line', {},
    line.timestamp ? h('span.dbsetup-log-time', {}, shortTime(line.timestamp)) : null,
    h('span', { class: line.stream === 'stderr' ? 'dbsetup-log-err' : undefined }, line.text)));
  // 너무 길어지면 앞을 버린다. 브라우저가 느려지는 것이 로그를 다 들고 있는
  // 것보다 나쁘다 — 오래된 줄은 어차피 위로 밀려 아무도 보지 않는다.
  while (box.childElementCount > 2000) box.removeChild(box.firstChild);
  if (atBottom) box.scrollTop = box.scrollHeight;
}

function shortTime(ts) {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts.slice(11, 19) : d.toLocaleTimeString();
}

// removeInstance는 지우기 전에 무엇이 사라지는지 묻는다.
//
// 컨테이너와 데이터는 되돌릴 수 있는 정도가 다르다. 컨테이너는 다시 만들면
// 되지만 볼륨은 지우면 끝이라, 한 번의 확인으로 둘 다 지우게 두지 않는다.
async function removeInstance(in_, act) {
  const recordOnly = in_.status === 'removed' && !in_.containerId;
  if (recordOnly) {
    const ok = await confirmDialog({
      title: '기록 지우기',
      message: `"${in_.name}" 의 기록을 지웁니다. 컨테이너는 이미 없습니다.`,
      confirmLabel: '지우기',
      danger: true,
    });
    if (!ok) return;
    await act('기록을 지웠습니다', () => api.del(`/docker/instances/${in_.id}`));
    return;
  }

  const dropData = checkbox('데이터도 함께 지웁니다 (되돌릴 수 없습니다)', {});
  const ok = await confirmDialog({
    title: 'DB 컨테이너 지우기',
    message: `"${in_.name}" 컨테이너를 지웁니다.`,
    details: h('div', {},
      in_.volumeName
        ? h('div', {}, dropData,
          h('p.field-help', {}, `볼륨 ${in_.volumeName} 에 데이터가 있습니다. `
            + '끄면 볼륨은 남으므로, 같은 이름으로 다시 만들면 그 데이터를 이어서 씁니다.'))
        : h('p.field-help', {}, '이 DB 는 데이터를 남기지 않으므로 함께 사라집니다.'),
      in_.connectionId
        ? h('p.field-help', {}, '등록된 커넥션은 그대로 남습니다. '
          + '더 쓰지 않을 것이면 DB 커넥션 화면에서 따로 지우세요.')
        : null),
    confirmLabel: '지우기',
    danger: true,
  });
  if (!ok) return;
  const keep = !dropData.querySelector('input').checked;
  await act(
    keep ? '컨테이너를 지웠습니다 (데이터는 남겼습니다)' : '컨테이너와 데이터를 지웠습니다',
    () => api.del(`/docker/instances/${in_.id}?keepData=${keep}`),
  );
}

// ---------- 상태 ----------

// statusStrip은 잘 되고 있을 때의 한 줄이다.
//
// 표를 접어 두는 이유: 확인이 끝난 정보가 화면 위쪽을 차지하면 정작 할 일이
// 아래로 밀린다. 그래도 없애지는 않는다 — 컨테이너가 뜨지 않을 때 가장 먼저
// 볼 곳이 이 표다.
function statusStrip(st) {
  return h('details.card.dbsetup-strip', {},
    h('summary', {},
      badge('쓸 수 있음', 'success'),
      h('span', {}, `도커 ${st.version} · ${st.os}/${st.architecture} · `
        + `컨테이너 ${st.containers}개 · ${st.cpus}코어 · ${gib(st.memTotal)}`)),
    h('dl.kv', {}, ...statusRows(st).flatMap(([k, v]) => [
      h('dt', {}, k), h('dd', {}, String(v)),
    ])),
    (st.warnings ?? []).length
      ? h('div', {},
        h('h3.erd-sub', {}, '데몬이 남긴 경고'),
        h('ul.field-help', {}, ...st.warnings.map((w) => h('li', {}, w))))
      : null);
}

// gateCard는 이중 게이트에 막혔을 때다.
//
// 서버 스위치와 사용자 권한을 갈라서 말한다. 둘은 고치는 사람이 다르다 —
// 스위치는 프로세스를 띄우는 사람이, 권한은 관리자가 고친다. 뭉쳐서 "쓸 수
// 없습니다"라고 하면 누구에게 말해야 하는지 알 수 없다.
function gateCard(err) {
  const code = err?.code ?? err?.body?.error ?? '';
  if (code === 'docker_disabled') {
    return h('div.card', {},
      h('h2', {}, badge('꺼짐', 'warn'), ' 이 서버는 도커 기능이 꺼져 있습니다'),
      h('p.field-help', {},
        '프로세스를 띄우는 사람이 켜야 합니다. 권한 설정으로는 켤 수 없습니다 — '
        + '도커 소켓에 닿는다는 것은 그 기계에서 무엇이든 할 수 있다는 뜻이라, '
        + '화면의 클릭으로 켜질 일이 아닙니다.'),
      h('h3.erd-sub', {}, '켜는 방법'),
      h('pre.code-block', {},
        'docker compose 로 돌고 있다면 dbstudio 서비스에:\n'
        + '\n'
        + '    environment:\n'
        + '      DBSTUDIO_ALLOW_DOCKER: "true"\n'
        + '    volumes:\n'
        + '      - /var/run/docker.sock:/var/run/docker.sock\n'
        + '\n'
        + '바이너리로 돌고 있다면:\n'
        + '\n'
        + '    ./dbstudio -allow-docker'),
      h('p.field-help', {},
        '켠 뒤에도 사용자에게 ', h('strong', {}, 'DB 컨테이너 관리'),
        ' 권한을 따로 줘야 합니다(권한 화면).'),
      socketNote());
  }
  return h('div.card', {},
    h('h2', {}, badge('권한 없음', 'danger'), ' DB 컨테이너 관리 권한이 없습니다'),
    h('p.field-help', {},
      err?.message ?? '관리자에게 “DB 컨테이너 관리” 권한을 요청하세요.'));
}

function statusRows(st) {
  return [
    ['도커', `${st.version} (API ${st.apiVersion})`],
    ['호스트', `${st.os} / ${st.architecture}`],
    ['컨테이너', `${st.containers}개 (도는 중 ${st.running}개)`],
    ['이미지', `${st.images}개`],
    ['CPU · 메모리', `${st.cpus}코어 · ${gib(st.memTotal)}`],
    ['스웜', swarmLabel(st.swarmState)],
    ['소켓', st.socket],
  ];
}

// statusCard는 데몬 상태다(못 쓸 때).
function statusCard(st, ourAPI) {
  if (!st) return emptyState('상태를 읽지 못했습니다');

  if (!st.reachable) {
    return h('div.card', {},
      h('h2', {}, badge('닿지 못함', 'danger'), ' 도커에 닿지 못했습니다'),
      h('dl.kv', {},
        h('dt', {}, '소켓'), h('dd', {}, h('code', {}, st.socket)),
        h('dt', {}, '이유'), h('dd', {}, st.reason ?? '알 수 없음')),
      socketNote());
  }

  return h('div.card', {},
    h('h2', {}, badge('버전 안 맞음', 'danger'), ' 도커에 닿았습니다'),
    h('p.erd-panel-danger', {}, st.reason),
    h('p.field-help', {}, `이 앱이 말하는 버전: ${ourAPI}`),
    h('dl.kv', {}, ...statusRows(st).flatMap(([k, v]) => [
      h('dt', {}, k), h('dd', {}, String(v)),
    ])));
}

// socketNote는 소켓을 넣는 사람이 반드시 부딪히는 것을 미리 적는다.
//
// 이 앱은 nonroot(65532)로 돌고 도커 소켓은 보통 root:docker 다. 소켓을
// 마운트해도 열 수 없고, 그 오류는 "permission denied" 한 줄이다.
function socketNote() {
  return h('details.field-help', {},
    h('summary', {}, '소켓을 넣었는데도 권한 오류가 난다면'),
    h('p', {},
      '이 앱은 nonroot(uid 65532)로 돌고, 도커 소켓은 보통 root:docker 입니다. '
      + '마운트만 해서는 열 수 없습니다. 호스트의 docker 그룹 gid 를 확인해 '
      + '컨테이너에 그 그룹을 주세요.'),
    h('pre.code-block', {},
      'getent group docker      # 예: docker:x:988:\n'
      + '\n'
      + '    services:\n'
      + '      dbstudio:\n'
      + '        group_add: ["988"]'),
    h('p', {},
      '소켓을 그대로 주는 것이 부담스러우면 소켓 프록시를 앞에 두고 필요한 '
      + '경로만 여는 방법도 있습니다. 그때는 -docker-socket 으로 프록시의 '
      + '소켓을 가리키세요.'));
}

function swarmLabel(state_) {
  switch (state_) {
    case 'active': return '켜져 있음 (스웜 모드)';
    case 'pending': return '준비 중';
    case 'locked': return '잠김';
    case 'error': return '오류';
    default: return '꺼져 있음';
  }
}

function gib(bytes) {
  if (!bytes || bytes <= 0) return '알 수 없음';
  return `${(bytes / (1024 ** 3)).toFixed(1)} GiB`;
}

// 메뉴가 보이려면 서버 스위치와 권한이 둘 다 있어야 한다(auth_handlers.go).
// 그런데도 주소를 직접 치고 들어올 수 있으므로 화면이 다시 확인한다 —
// 실제 관문은 서버이고 이것은 안내다.
export function dockerMenuVisible() {
  return Boolean(state.permissions?.dockerManage);
}

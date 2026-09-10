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
  openModal, confirmDialog, toast, toastError, relativeTime, copyToClipboard,
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
  const cluster = recipe.id === 'clickhouse' ? clusterFields() : null;
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
    cluster ? cluster.node : null,
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
    const shape = cluster?.value();
    if (shape) payload.cluster = shape;

    // 만들기 전에 계획을 받아 경고를 보여 준다. 이 만들기는 되돌리는 값이
    // 크다 — 컨테이너 하나와 볼륨 하나가 생기고, 잘못이면 지우는 것까지 해야
    // 한다. 값이 잘못되었으면 여기서 걸려서 아무것도 만들어지지 않는다.
    // 클러스터는 계획이 하나가 아니라 미리보기 경로가 다르다. 여기서는
    // 만들기 응답에 담겨 오는 경고를 보여 준다.
    let warnings = [];
    if (!shape) {
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
      warnings = plan.plan?.warnings ?? [];
    }

    if (warnings.length) {
      const ok = await confirmDialog({
        title: '이대로 만들까요?',
        message: warnings.join('\n'),
        confirmLabel: '만들기',
      });
      if (!ok) return;
    }

    try {
      const res = await api.post('/docker/instances', payload);
      close();
      if (res.nodes) {
        // 클러스터의 경고는 만들고 나서야 온다. 토스트로 흘려보내면 읽히지
        // 않으므로 창으로 보여 준다 — 한 기계에 다 뜬다는 것 같은 것은
        // 만든 사람이 반드시 알아야 한다.
        await confirmDialog({
          title: `${name} 클러스터를 만들고 있습니다`,
          message: `노드 ${res.nodes}대를 순서대로 띄웁니다. 몇 분 걸립니다.`,
          details: (res.warnings ?? []).length
            ? h('div', {}, ...res.warnings.map((w) => h('p.field-help', {}, `• ${w}`)))
            : null,
          confirmLabel: '알겠습니다',
        });
      } else {
        toast(`${name} 을(를) 만들고 있습니다`, 'info');
      }
      renderDBSetup(root);
    } catch (err) {
      toastError(err);
    }
  };
}

// clusterFields는 ClickHouse 를 클러스터로 세울 때의 칸이다.
//
// ── 왜 ClickHouse 뿐인가 ────────────────────────────────────────────
// 클러스터는 DB 마다 뜻이 다르다. PostgreSQL 의 복제와 ClickHouse 의 샤딩은
// 같은 말이 아니고, 만드는 것도 다루는 것도 다르다. 하나를 제대로 하는 편이
// 여럿을 어설프게 하는 것보다 낫다.
function clusterFields() {
  const on = checkbox('클러스터로 만들기 (샤딩·복제)', { checked: false });
  const shards = select([1, 2, 3, 4].map((n) => ({ value: String(n), label: `${n}개` })),
    { value: '2' });
  const replicas = select([1, 2, 3].map((n) => ({ value: String(n), label: `${n}개` })),
    { value: '2' });
  const detail = h('div.dbsetup-cluster-opts', { hidden: true },
    field('샤드 (데이터를 나눌 조각)', shards,
      '조각마다 다른 데이터가 들어갑니다. 늘리면 쓰기와 조회가 나뉩니다'),
    field('레플리카 (조각마다 둘 복제본)', replicas,
      '같은 데이터를 몇 벌 둘지입니다. 2 이상이면 복제를 맞출 Keeper 가 함께 뜹니다'),
    h('p.field-help', {},
      '노드는 샤드 × 레플리카 만큼 뜨고, 모두 이 기계 한 대에 뜹니다. '
      + '커넥션은 첫 노드 하나만 등록되며 DDL 에 ON CLUSTER 가 붙습니다.'));

  on.querySelector('input').onchange = (e) => { detail.hidden = !e.target.checked; };
  return {
    node: h('div.dbsetup-cluster', {}, on, detail),
    value: () => (on.querySelector('input').checked
      ? { shards: Number(shards.value), replicas: Number(replicas.value) }
      : null),
  };
}

// fieldFor는 서버가 준 필드 하나를 칸으로 그린다.
//
// 종류별 분기를 여기 한 곳에 두는 이유: 화면이 DB 마다 칸을 적어 두면 새 DB 를
// 더할 때 화면도 고쳐야 하고, 그때 한쪽만 고치면 값이 조용히 빠진다.
//
// current 를 주면 그 값으로 채운다(고치기). 주지 않으면 기본값이다(만들기).
// edit 이면 **이 칸을 고치면 무엇이 필요한지**를 칸 옆에 붙인다 — 누르고 나서
// 알게 되는 것과 적기 전에 아는 것은 다른 일이다.
function fieldFor(f, values, opts = {}) {
  const { current = null, edit = false } = opts;
  const initial = current && current[f.key] !== undefined ? current[f.key] : (f.default ?? '');
  const set = (v) => { values[f.key] = v; };
  if (initial !== '' && values[f.key] === undefined) set(initial);

  let control;
  switch (f.kind) {
    case 'bool': {
      const on = String(initial) === 'true';
      const box = checkbox(f.label, { checked: on });
      box.querySelector('input').onchange = (e) => set(e.target.checked ? 'true' : 'false');
      set(on ? 'true' : 'false');
      return h('div.dbsetup-bool', {}, box,
        edit ? applyBadge(f) : null,
        f.help ? h('span.field-help', {}, f.help) : null);
    }
    case 'select':
      control = select((f.choices ?? []).map((v) => ({ value: v, label: v })),
        { value: initial, onchange: (e) => set(e.target.value) });
      break;
    case 'password':
      control = input({
        type: 'password',
        // 고칠 때는 값을 채우지 않는다. 저장된 비밀번호를 화면에 되돌려 주면
        // 그 화면을 보는 것이 곧 비밀번호를 보는 것이 된다.
        placeholder: edit ? '그대로 두려면 비워 두세요' : (f.placeholder ?? ''),
        autocomplete: 'new-password', oninput: (e) => set(e.target.value),
      });
      if (edit) delete values[f.key];
      break;
    case 'number':
      control = input({
        type: 'number', value: initial, placeholder: f.placeholder ?? '',
        min: f.min ?? undefined, max: f.max ?? undefined,
        oninput: (e) => set(e.target.value),
      });
      break;
    default:
      control = input({
        value: initial, placeholder: f.placeholder ?? '',
        oninput: (e) => set(e.target.value),
      });
  }
  const label = f.required && !edit ? `${f.label} *` : f.label;
  const wrapped = field(label, control, f.help);
  if (edit) wrapped.querySelector('.field-label').appendChild(applyBadge(f));
  return wrapped;
}

// applyBadge는 이 칸을 고치면 무엇이 필요한지다.
//
// init 을 가장 눈에 띄게 둔다. 그것만이 "고쳐도 소용없다"이고, 나머지는
// "이만큼 하면 된다"이기 때문이다.
function applyBadge(f) {
  const map = {
    live: ['바로 적용', 'success'],
    restart: ['재시작 필요', 'warn'],
    recreate: ['다시 만들기 필요', 'warn'],
    init: ['지금 DB 에는 적용 안 됨', 'danger'],
  };
  const [label, kind] = map[f.apply ?? ''] ?? ['다시 만들기 필요', 'warn'];
  const help = f.apply === 'init'
    ? '이 값은 처음 만들 때 한 번만 쓰입니다. 고쳐도 지금 DB 는 그대로입니다'
    : label;
  const b = badge(label, kind);
  b.classList.add('dbsetup-apply');
  b.title = help;
  return b;
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
      badge(`${instances.length}개`, 'neutral'),
      h('button.btn.btn-small.notice-link', {
        type: 'button',
        onclick: () => openCompose(withProject('/docker/compose'), '프로젝트 — compose.yml'),
      }, icon('save'), 'compose.yml 로 내보내기')),
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
    h('td', {},
      `${in_.kind} ${in_.version}`,
      // 클러스터의 노드는 그렇게 보여야 한다. 이름만 보고는 다섯 줄이 한
      // 묶음인지, 서로 다른 DB 다섯 개인지 알 수 없다.
      in_.cluster
        ? h('div.field-help', {}, in_.role === 'keeper'
          ? `${in_.cluster} · 조정자`
          : `${in_.cluster} · 샤드 ${in_.shard} 레플리카 ${in_.replica}`)
        : null),
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
      // 커넥션 화면은 목록 하나다(/connections/:id 라는 경로는 없다).
      // 이 앱의 다른 화면들도 모두 목록으로 보낸다 — 여기서만 다른 규칙을
      // 만들면 그 경로가 없다는 것을 눌러 보고서야 알게 된다.
      in_.connectionId
        ? h('a.btn.btn-small', { href: '/connections' },
          icon('link'), `커넥션 목록에서 ${in_.name} 보기`)
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
      !gone
        ? h('button.btn', {
          type: 'button',
          onclick: () => openEditModal(root, summary, detail, reload),
        }, icon('settings'), '설정 고치기')
        : null,
      h('button.btn', {
        type: 'button',
        onclick: () => openCompose(`/docker/instances/${in_.id}/compose`,
          `${in_.name} — compose.yml`),
      }, icon('save'), 'compose.yml'),
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

// openEditModal은 만든 DB 의 설정을 고친다.
//
// ── 왜 "무엇이 일어나는지"를 먼저 보여 주는가 ───────────────────────
// 고칠 수 있는 값이라고 다 같은 값이 아니다. 메모리 상한은 도커가 컨테이너를
// 그대로 두고 바꿔 주지만, 실행 인자는 다시 만들어야 하고, 계정과 비밀번호는
// **다시 만들어도 안 바뀌는** DB 가 있다(PostgreSQL·MySQL·MariaDB·MongoDB·
// MS-SQL 은 첫 실행에서만 쓴다 — 실제로 재 봤다).
//
// 그것을 말하지 않고 "저장했습니다"라고만 하면, 사람은 바뀐 줄 알고 새
// 비밀번호로 접속하다 막힌다.
async function openEditModal(root, summary, detail, onDone) {
  const recipe = detail.recipe;
  const in_ = detail.instance;
  if (!recipe) {
    toast('이 DB 의 레시피를 찾을 수 없어 고칠 수 없습니다', 'error');
    return;
  }

  const values = {};
  const current = in_.values ?? {};
  const basic = recipe.fields.filter((f) => !f.advanced && !f.hidden);
  const advanced = recipe.fields.filter((f) => f.advanced && !f.hidden);
  const opts = { current, edit: true };

  const body = h('div', {},
    h('p.field-help', {},
      '칸 옆의 표시가 그 값을 고쳤을 때 무엇이 필요한지입니다. '
      + '비밀번호는 그대로 두려면 비워 두세요.'),
    ...basic.map((f) => fieldFor(f, values, opts)),
    advanced.length
      ? h('details.dbsetup-advanced', { open: true },
        h('summary', {}, '자세한 설정'),
        ...advanced.map((f) => fieldFor(f, values, opts)))
      : null);

  const submit = h('button.btn.btn-primary', { type: 'button' }, icon('save'), '고치기');
  const close = openModal({
    title: `${in_.name} 설정 고치기`, body, width: 640,
    footer: (closeFn) => [
      h('button.btn', { type: 'button', onclick: closeFn }, '취소'),
      submit,
    ],
  });

  submit.onclick = async () => {
    // 바꾸지 않은 칸은 보내지 않는다. 다 보내면 서버가 "바뀌었다"로 읽고
    // 컨테이너를 괜히 다시 만든다.
    const payload = { values: changedOnly(recipe, current, values) };
    if (!Object.keys(payload.values).length) {
      toast('바뀐 것이 없습니다', 'info');
      return;
    }

    let preview;
    try {
      submit.disabled = true;
      preview = await api.post(`/docker/instances/${in_.id}/changes`, payload);
    } catch (err) {
      submit.disabled = false;
      toastError(err);
      return;
    }
    submit.disabled = false;

    const ok = await confirmDialog({
      title: '이대로 고칠까요?',
      message: preview.needsText,
      details: changeList(preview),
      confirmLabel: '고치기',
      danger: preview.needed === 'recreate',
    });
    if (!ok) return;

    try {
      const res = await api.patch(`/docker/instances/${in_.id}`, payload);
      close();
      const skipped = (res.initOnly ?? []).length;
      toast(skipped
        ? `고쳤습니다. ${skipped}개는 지금 DB 에 적용되지 않았습니다`
        : '고쳤습니다', skipped ? 'warn' : 'success');
      onDone?.();
      renderDBSetup(root);
    } catch (err) {
      toastError(err);
      onDone?.();
    }
  };
}

// changedOnly는 실제로 바뀐 칸만 고른다.
function changedOnly(recipe, current, values) {
  const out = {};
  for (const f of recipe.fields) {
    const v = values[f.key];
    if (v === undefined) continue;
    // 비밀번호는 비워 두면 "그대로"다. 빈 값을 보내면 서버가 지우려는 것으로 읽는다.
    if (f.secret && v === '') continue;
    // 적어 두지 않은 칸의 지금 값은 **기본값**이다. 빈 값으로 보면 손대지
    // 않은 칸이 전부 "바뀜"으로 잡히고, 그중 하나가 다시 만들기를 요구하면
    // 컨테이너가 괜히 다시 만들어진다.
    const was = current[f.key] ?? f.default ?? '';
    if (String(v) === String(was)) continue;
    out[f.key] = String(v);
  }
  return out;
}

// changeList는 확인 창에 보일 변경 목록이다.
//
// 적용되지 않는 것을 맨 위에 따로 모은다. 목록 가운데 섞여 있으면 그것만
// 다르다는 것이 읽히지 않는다.
function changeList(preview) {
  const initOnly = preview.initOnly ?? [];
  const rest = (preview.changes ?? []).filter((c) => c.apply !== 'init');
  return h('div', {},
    initOnly.length
      ? h('div.notice.notice-danger', {}, icon('alert'),
        h('div', {},
          h('strong', {}, '이 값들은 지금 DB 에 적용되지 않습니다'),
          h('p', {}, '처음 만들 때 한 번만 쓰이는 값입니다. 바꾸려면 데이터를 지우고 '
            + '새로 만들거나, DB 안에서 직접 바꿔야 합니다.'),
          ...initOnly.map((c) => h('p', {}, `• ${c.label}: ${c.from || '(없음)'} → ${c.to}`))))
      : null,
    rest.length
      ? h('dl.kv', {}, ...rest.flatMap((c) => [
        h('dt', {}, c.label),
        h('dd', {}, `${c.from || '(없음)'} → ${c.to}`),
      ]))
      : null);
}

// ---------- compose.yml 내보내기 ----------

// openCompose는 만든 것을 compose.yml 또는 스웜 스택 파일로 보여 준다.
//
// ── 왜 이 기능이 필요한가 ───────────────────────────────────────────
// 화면에서 정한 것을 **가져갈 수 있어야** 한다. 이 앱 없이도 같은 DB 를 띄울 수
// 있어야 하고, 그 파일을 저장소에 넣어 검토할 수 있어야 한다. 그렇지 않으면
// 여기서 만든 DB 는 이 앱 안에서만 존재하는 것이 되고, 그것은 사람을 묶어 두는
// 종류의 편리함이다.
//
// 두 형식을 한 창에서 바꿔 보게 한 이유: 둘은 같은 것을 말하는 다른 표기다.
// 창을 갈라 두면 어느 것이 지금 것인지 헷갈리고, 스웜 파일을 compose 로
// 돌리거나 그 반대를 하게 된다 — 둘 다 오류 없이 다르게 동작한다.
async function openCompose(path, title) {
  let format = 'compose';
  const body = h('div.dbsetup-detail');
  const foot = h('div.dbsetup-actions');
  let current = null;

  const close = openModal({ title, width: 780, body, footer: () => foot });

  const load = async () => {
    mount(body, spinner('만드는 중…'));
    try {
      current = await api.get(path + (path.includes('?') ? '&' : '?') + 'format=' + format);
    } catch (err) {
      mount(body, h('div.notice.notice-danger', {}, icon('alert'),
        h('span', {}, err.message ?? '만들지 못했습니다')));
      return;
    }
    draw();
  };

  function tab(value, label, help) {
    return h('button', {
      type: 'button',
      class: format === value ? 'btn btn-small btn-primary' : 'btn btn-small',
      title: help,
      onclick: () => {
        if (format === value) return;
        format = value;
        load();
      },
    }, label);
  }

  function draw() {
    const res = current;
    const files = Object.entries(res.files ?? {});
    mount(body,
      h('div.dbsetup-format', {},
        tab('compose', 'docker compose', '한 대에서 띄웁니다'),
        tab('stack', 'docker swarm', 'docker stack deploy 로 띄웁니다'),
        h('span.field-help', {}, `서비스 ${res.services}개 · ${res.filename}`)),
      (res.notes ?? []).length
        ? h('div.notice.notice-warn', {}, icon('alert'),
          h('div', {}, ...res.notes.map((n) => h('p', {}, n))))
        : null,
      (res.skipped ?? []).length
        ? h('div.notice.notice-danger', {}, icon('alert'),
          h('div', {},
            h('strong', {}, '빠진 것'),
            ...res.skipped.map((n) => h('p', {}, n))))
        : null,
      h('pre.code-block.dbsetup-compose', {}, res.yaml),
      res.envExample
        ? h('div', {},
          h('h3.erd-sub', {}, format === 'stack' ? '넣어야 하는 환경변수' : '.env'),
          h('p.field-help', {}, format === 'stack'
            ? 'docker stack deploy 는 .env 를 읽지 않습니다. 아래 이름들을 환경변수로 '
              + '넣고 배포하세요 — 넣지 않으면 배포가 멈춥니다.'
            : '비밀번호는 파일에 넣지 않았습니다. 만들 때 정한 값을 여기 채워 넣으세요.'),
          h('pre.code-block.dbsetup-compose', {}, res.envExample))
        : null,
      files.length
        ? h('div', {},
          h('h3.erd-sub', {}, '함께 저장할 설정 파일'),
          h('p.field-help', {}, `${res.filename} 옆에 같은 경로로 두어야 합니다. `
            + '없으면 도커가 그 자리에 빈 디렉터리를 만들고, DB 는 설정 없이 뜹니다.'),
          ...files.map(([p]) => h('div.field-help', {}, h('code', {}, p))))
        : null);

    mount(foot,
      h('button.btn', {
        type: 'button',
        onclick: () => {
          copyToClipboard(res.yaml);
          toast(`${res.filename} 을 복사했습니다`, 'success');
        },
      }, icon('copy'), '복사'),
      h('button.btn.btn-primary', {
        type: 'button', onclick: () => saveComposeBundle(res),
      }, icon('save'), '파일로 저장'),
      h('span.dbsetup-actions-gap'),
      h('button.btn', { type: 'button', onclick: close }, '닫기'));
  }

  await load();
}

// saveComposeBundle은 파일들을 하나씩 내려받게 한다.
//
// 압축해서 하나로 주지 않는 이유: 그러려면 압축 라이브러리를 화면에 들여야
// 하는데, 파일은 많아야 서너 개다. 대신 경로를 이름에 담아 어디에 두어야
// 하는지가 파일 이름만 봐도 보이게 한다.
function saveComposeBundle(res) {
  downloadText(res.filename ?? 'compose.yaml', res.yaml);
  // 스웜은 .env 를 읽지 않는다. 읽지 않는 파일을 함께 내려받게 하면 그것을
  // 두고서 "넣었는데 왜 안 되지"가 된다 — 이름 목록은 창에 이미 보인다.
  if (res.envExample && res.format !== 'stack') downloadText('.env', res.envExample);
  for (const [path, body] of Object.entries(res.files ?? {})) {
    downloadText(path.replace(/\//g, '__'), body);
  }
  const extra = Object.keys(res.files ?? {}).length;
  toast(extra
    ? `내려받았습니다. 설정 파일 ${extra}개는 이름의 __ 를 / 로 되돌려 두세요`
    : '내려받았습니다', 'info');
}

// downloadText는 만든 문자열을 파일로 내려받게 한다.
// 서버를 거치지 않는다 — 내용이 이미 브라우저에 있고, 왕복하면 같은 것을 두 번 만든다.
function downloadText(filename, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // 즉시 해제하면 브라우저가 저장을 시작하기 전에 사라질 수 있다.
  setTimeout(() => URL.revokeObjectURL(url), 10000);
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

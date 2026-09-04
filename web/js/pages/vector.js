// 벡터 DB(Qdrant · Pinecone · pgvector) 화면.
//
// DB 화면과 나눈 이유: 여기에는 표도 SQL도 없다. 대신 차원·거리 함수·색인·이웃이
// 있고, 이 화면에서 얻고 싶은 답은 셋이다 — 무엇이 얼마나 들어 있고 색인은
// 준비됐는가, 이 안에 실제로 무엇이 담겨 있는가, 이 둘은 왜 가깝다고 나오는가.
//
// **읽기 전용이다.** 임베딩은 다른 파이프라인이 만들어 넣는 것이라, 이 화면에서
// 한 줄을 고치면 그 파이프라인이 다음에 덮어쓰거나 영영 어긋난 채 남는다.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import { state } from '../core/store.js';
import {
  h, mount, icon, input, textarea, spinner, emptyState, pageHeader,
  badge, envBadge, toast, copyToClipboard,
} from '../core/ui.js';
import { navigate } from '../core/router.js';
import { groupedSelect } from '../core/connpick.js';
import { setScreenDetail, screenConn } from '../core/screen.js';
import { errorPanel } from './users.js';

// METRIC_LABELS는 거리 함수의 사람 말 이름이다. 서버가 한 어휘로 모아 주므로
// 화면은 세 가지만 알면 된다.
const METRIC_LABELS = {
  cosine: '코사인',
  euclid: '유클리드',
  dot: '내적',
};

export async function renderVector(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  try {
    conns = await api.get(withProject('/connections/'));
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 벡터 능력이 켜진 종류 + PostgreSQL. PostgreSQL 을 함께 두는 이유는
  // pgvector 가 **확장**이라 깔려 있는지는 붙어 봐야 알기 때문이다. 목록에서
  // 미리 감추면 확장을 방금 깐 사람이 그 사실을 알 방법이 없다.
  const usable = conns.items.filter((i) => {
    if (!i.accessible) return false;
    const info = state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind);
    return info?.capabilities?.vector || i.connection.kind === 'postgres';
  });

  if (usable.length === 0) {
    mount(outlet,
      pageHeader('벡터 DB', 'Qdrant · Pinecone · pgvector'),
      emptyState('등록된 벡터 DB가 없습니다. Qdrant나 Pinecone을 커넥션으로 등록하거나, '
        + 'pgvector 확장이 깔린 PostgreSQL 커넥션을 쓰세요.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')),
    );
    return;
  }

  const selectedId = query.get('conn') || usable[0].connection.id;
  const current = usable.find((i) => i.connection.id === selectedId) ?? usable[0];
  const conn = current.connection;

  const connSelect = groupedSelect({ usable, currentId: conn.id });
  connSelect.addEventListener('change', () => {
    navigate(`/vector?conn=${encodeURIComponent(connSelect.value)}`);
  });

  const body = h('div');
  mount(outlet,
    pageHeader('벡터 DB', `${conn.name} 의 임베딩을 봅니다`, [
      h('button.btn', { type: 'button', onclick: () => load() }, icon('refresh'), '다시 읽기'),
    ]),
    h('div.card.filter-bar', {},
      h('label.field.field-inline', {}, h('span.field-label', {}, '대상'), connSelect),
      envBadge(conn.environment),
      h('div.filter-sep'),
      h('a.btn.btn-small', { href: `/monitor?conn=${encodeURIComponent(conn.id)}` },
        icon('activity'), '모니터링'),
    ),
    body,
  );

  async function load() {
    mount(body, spinner(`${conn.name} 을(를) 읽는 중…`));
    try {
      const res = await api.get(`/connections/${conn.id}/vector`);
      mount(body, ...vectorView(conn, res, query.get('collection') ?? ''));
      setScreenDetail(() => screenBits(conn, res));
    } catch (err) {
      // pgvector 확장이 없을 때가 여기로 온다. 그것은 서버 장애가 아니라
      // "여기에는 볼 것이 없다"이므로, 무엇을 하면 되는지가 오류에 들어 있다.
      mount(body, errorPanel(err));
    }
  }
  await load();
}

// screenBits는 어시스턴트에게 보고할 이 화면의 사실들이다.
function screenBits(conn, res) {
  const ov = res.overview ?? {};
  return [
    screenConn(conn, '보고 있는 벡터 DB'),
    `종류: ${res.kind}${ov.version ? ` (${ov.version})` : ''}`,
    `컬렉션 ${(ov.collections ?? []).length}개`,
  ];
}

function vectorView(conn, res, initial) {
  const ov = res.overview ?? {};
  const feats = res.features ?? {};
  const collections = ov.collections ?? [];
  const detail = h('div');

  const open = (name) => {
    const col = collections.find((c) => c.name === name);
    if (!col) {
      mount(detail);
      return;
    }
    mount(detail, collectionCard(conn, col, feats));
  };

  const out = [
    overviewCard(ov, res.kind),
    collectionsCard(collections, open, initial),
    detail,
  ];
  if (initial) open(initial);
  return out;
}

function overviewCard(ov, kind) {
  return h('div.card', {},
    h('div.card-title', {},
      h('span', {}, '개요'),
      badge(kind, 'info'),
      ov.version ? h('span.muted.small', {}, ov.version) : null,
    ),
    h('div.vec-facts', {},
      fact('컬렉션', String((ov.collections ?? []).length)),
      ...(ov.facts ?? []).map((f) => fact(f.label, f.value, f.help)),
    ),
    // 왜 이 값이 비어 있는지 말한다. 빈칸만 보여주면 사용자는 앱을 의심한다.
    ...(ov.notes ?? []).map((n) => h('p.notice.notice-warn', {}, icon('alert'), n)),
  );
}

function fact(label, value, help) {
  return h('div.vec-fact', { title: help ?? '' },
    h('span.vec-fact-label', {}, label),
    h('span.vec-fact-value', {}, value),
  );
}

// statusBadge는 컬렉션 상태다.
//
// unknown 을 위험으로 그리지 않는다 — 모른다는 것과 준비되지 않았다는 것은
// 다르고, 종류에 따라 아예 알려주지 않는 값이다(Pinecone 이 그렇다).
function statusBadge(status) {
  switch (status) {
    case 'green': return badge('준비됨', 'success');
    case 'yellow': return badge('작업 중', 'warn');
    case 'red': return badge('오류', 'danger');
    default: return badge('알 수 없음', 'neutral');
  }
}

function collectionsCard(collections, open, initial) {
  if (collections.length === 0) {
    return h('div.card', {},
      h('h2.card-title', {}, '컬렉션'),
      emptyState('컬렉션이 없습니다.'));
  }
  return h('div.card', {},
    h('div.card-title', {}, h('span', {}, '컬렉션'),
      h('span.muted.small', {}, `${collections.length}개`)),
    h('table.table.vec-collections', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '이름'), h('th', {}, '차원'), h('th', {}, '거리'),
        h('th', {}, '벡터'), h('th', {}, '색인'), h('th', {}, '상태'), h('th', {}, ''))),
      h('tbody', {}, collections.map((c) => collectionRow(c, open, initial)))),
  );
}

function collectionRow(col, open, initial) {
  const count = (v) => (v < 0 ? h('span.muted', {}, '모름') : v.toLocaleString('ko-KR'));

  // 색인이 담긴 수보다 적으면 그 사실을 눈에 띄게 그린다. 그 차이가 "왜 느린가"의
  // 유일한 단서다 — 색인에 오르지 못한 벡터도 검색은 되지만 전수 조사로 떨어진다.
  const behind = col.indexed >= 0 && col.points > 0 && col.indexed < col.points;

  return h('tr', {
    class: col.name === initial ? 'is-selected' : '',
  },
    h('td', {}, h('strong', {}, col.name),
      col.indexType ? h('span.muted.small', {}, ` ${col.indexType}`) : null),
    h('td', {}, col.dimensions > 0 ? String(col.dimensions) : h('span.muted', {}, '모름')),
    h('td', {}, METRIC_LABELS[col.metric] ?? col.metric ?? '-'),
    h('td', {}, count(col.points)),
    h('td', {}, behind
      ? h('span.vec-behind', { title: '색인이 아직 따라오지 못했습니다' },
        count(col.indexed), icon('alert', 11))
      : count(col.indexed)),
    h('td', {}, statusBadge(col.status),
      col.fullness >= 0 ? badge(`${col.fullness.toFixed(1)}%`, 'neutral') : null),
    h('td', {}, h('button.btn.btn-small', {
      type: 'button', onclick: () => open(col.name),
    }, '열기')),
  );
}

// ---------- 컬렉션 상세 ----------

function collectionCard(conn, col, feats) {
  const card = h('div.card');
  const panel = h('div');
  let tab = 'browse';

  // 탭 사이에 오가는 값. 창(window) 이벤트로 주고받지 않는 이유: 탭을 바꾸면
  // 옛 화면이 사라지는데 리스너는 남아서, 화면을 몇 번 오가면 이미 없어진
  // 입력칸을 채우려다 죽는다. 이 객체는 카드가 살아 있는 동안만 산다.
  const picks = { similar: '', a: '', b: '' };
  const goto = (next) => { tab = next; render(); };

  const tabs = [
    ['browse', '훑어보기', 'list'],
    ['search', '이웃 찾기', 'search'],
    ['compare', '비교', 'workflow'],
  ];
  const render = () => {
    mount(card,
      h('div.card-title', {},
        h('span', {}, col.name),
        col.dimensions > 0 ? badge(`${col.dimensions}차원`, 'info') : null,
        badge(METRIC_LABELS[col.metric] ?? col.metric ?? '거리 미상', 'neutral'),
        statusBadge(col.status),
      ),
      col.note ? h('p.notice.notice-info', {}, icon('alert'), col.note) : null,
      (col.facts ?? []).length
        ? h('div.vec-facts', {}, col.facts.map((f) => fact(f.label, f.value, f.help)))
        : null,
      h('div.vec-tabs', {}, tabs.map(([id, label, ic]) =>
        h('button.btn.btn-small', {
          type: 'button',
          class: tab === id ? 'btn btn-small btn-active' : 'btn btn-small',
          onclick: () => { tab = id; render(); },
        }, icon(ic, 13), label))),
      panel,
    );
    if (tab === 'browse') mount(panel, browseView(conn, col, feats, picks, goto));
    else if (tab === 'search') mount(panel, searchView(conn, col, feats, picks, goto));
    else mount(panel, compareView(conn, col, picks));
  };
  render();
  return card;
}

// ---------- 훑어보기 ----------

function browseView(conn, col, feats, picks, goto) {
  const box = h('div.vec-panel');
  const load = async (cursor = '', acc = []) => {
    mount(box, spinner('벡터를 읽는 중…'));
    let res;
    try {
      const q = new URLSearchParams({ collection: col.name, limit: '50' });
      if (cursor) q.set('cursor', cursor);
      res = await api.get(`/connections/${conn.id}/vector/scroll?${q}`);
    } catch (err) {
      mount(box, errorPanel(err),
        feats.scrollNote ? h('p.field-help', {}, feats.scrollNote) : null);
      return;
    }
    const page = res.page ?? {};
    const points = acc.concat(page.points ?? []);
    mount(box,
      ...(page.notes ?? []).map((n) => h('p.notice.notice-info', {}, icon('alert'), n)),
      points.length === 0
        ? emptyState('이 컬렉션에 벡터가 없습니다.')
        : h('div.vec-points', {}, points.map((p) => pointCard(p, picks, goto))),
      page.next
        ? h('button.btn.btn-small', {
          type: 'button', onclick: () => load(page.next, points),
        }, '더 보기')
        : null,
    );
  };
  load();
  return box;
}

// pointCard는 점 하나다. 벡터는 앞머리만 그린다 —
// 1536개의 수를 늘어놓아도 사람은 그것을 읽지 않는다.
function pointCard(p, picks, goto) {
  const act = (title, label, run) => h('button.btn.btn-small.btn-ghost', {
    type: 'button', title, disabled: !p.id, onclick: run,
  }, label);

  return h('div.vec-point', {},
    h('div.vec-point-head', {},
      h('span.mono.vec-id', { title: p.id }, p.id || '(id 없음)'),
      p.dimensions ? h('span.muted.small', {}, p.dimensions + '차원') : null,
      // 점수가 0 인 것도 뜻이 있다(거리 0 = 같은 벡터). 0 을 "없음"으로 보면
      // 가장 중요한 결과가 사라진다. 방향(scoreKind)이 있는지로 가른다.
      p.scoreKind ? scoreBadge(p.score ?? 0, p.scoreKind) : null,
      h('div.vec-point-actions', {},
        act('이 벡터와 비슷한 것 찾기', icon('search', 12),
          () => { picks.similar = p.id; goto('search'); }),
        act('비교의 A 로', 'A', () => { picks.a = p.id; goto('compare'); }),
        act('비교의 B 로', 'B', () => { picks.b = p.id; goto('compare'); }),
        act('id 복사', icon('copy', 12),
          () => copyToClipboard(p.id).then(() => toast('id 를 복사했습니다', 'success'))),
      ),
    ),
    sparkline(p.vector, p.truncated),
    payloadView(p.payload),
  );
}

// sparkline은 벡터 앞머리를 막대로 그린다.
//
// 수를 늘어놓지 않는 이유: 0.0231 같은 수 열여섯 개는 읽히지 않지만, 막대의
// 모양은 한눈에 견줄 수 있다. 정확한 값이 필요한 자리는 비교 화면이다.
function sparkline(values, truncated) {
  if (!values || values.length === 0) {
    return h('p.muted.small', {}, '벡터를 함께 받지 않았습니다');
  }
  const max = Math.max(...values.map((v) => Math.abs(v)), 1e-9);
  return h('div.vec-spark', { title: values.map((v) => v.toFixed(4)).join(', ') },
    ...values.map((v) => h('span.vec-bar', {
      class: v < 0 ? 'vec-bar is-neg' : 'vec-bar',
      style: `height:${Math.max(2, (Math.abs(v) / max) * 100)}%`,
    })),
    truncated ? h('span.vec-spark-more', {}, '…') : null,
  );
}

function payloadView(payload) {
  const keys = Object.keys(payload ?? {});
  if (keys.length === 0) return null;
  return h('div.vec-payload', {}, keys.slice(0, 6).map((k) =>
    h('span.vec-kv', {},
      h('span.muted', {}, k),
      h('span', {}, truncate(String(payload[k] ?? ''), 60)))));
}

function truncate(s, max) {
  return s.length > max ? `${s.slice(0, max)}…` : s;
}

// scoreBadge는 점수를 그린다.
//
// 방향을 반드시 함께 본다. 유사도는 클수록, 거리는 작을수록 가깝다 — 이것을
// 짐작으로 그리면 어떤 DB 에서는 가장 먼 것이 가장 가까운 것으로 보인다.
function scoreBadge(score, kind) {
  const near = kind === 'distance'
    ? '거리입니다 — 작을수록 가깝습니다'
    : '유사도입니다 — 클수록 가깝습니다';
  return h('span.badge.badge-info.vec-score', { title: near },
    kind === 'distance' ? '거리' : '유사도',
    h('span.mono', {}, Number(score).toFixed(4)));
}

// ---------- 이웃 찾기 ----------

function searchView(conn, col, feats, picks, goto) {
  const box = h('div.vec-panel');
  const idInput = input({
    value: picks.similar ?? '',
    placeholder: '기준이 될 점의 id (예: 42 또는 uuid)',
  });
  const vecInput = textarea({
    rows: 2,
    placeholder: '또는 벡터를 직접: 0.1, -0.2, 0.35 …',
  });
  const limitInput = input({ type: 'number', value: '20', min: '1', max: '200' });
  const filterInput = textarea({
    rows: 2, placeholder: '{"must":[{"key":"tag","match":{"value":"news"}}]}',
  });
  const results = h('div');

  async function run() {
    const payload = { collection: col.name, limit: Number(limitInput.value) || 20 };
    const id = idInput.value.trim();
    const raw = vecInput.value.trim();
    if (raw) {
      const nums = raw.split(/[\s,[\]]+/).filter(Boolean).map(Number);
      if (nums.some(Number.isNaN)) {
        toast('벡터에 숫자가 아닌 값이 있습니다', 'error');
        return;
      }
      // 차원이 다르면 서버가 거절한다. 그 전에 화면에서 말해 주면 왕복을 아낀다.
      if (col.dimensions > 0 && nums.length !== col.dimensions) {
        toast(`이 컬렉션은 ${col.dimensions}차원인데 ${nums.length}개를 넣었습니다`, 'error');
        return;
      }
      payload.vector = nums;
    } else if (id) {
      payload.id = id;
    } else {
      toast('id 를 고르거나 벡터를 넣으세요', 'error');
      return;
    }
    if (feats.filter && filterInput.value.trim()) {
      try {
        payload.filter = JSON.parse(filterInput.value);
      } catch {
        toast('필터가 올바른 JSON 이 아닙니다', 'error');
        return;
      }
    }

    mount(results, spinner('이웃을 찾는 중…'));
    try {
      const res = await api.post(`/connections/${conn.id}/vector/search`, payload);
      mount(results, resultView(res.result ?? {}, picks, goto));
    } catch (err) {
      mount(results, errorPanel(err));
    }
  }

  mount(box,
    h('div.vec-form', {},
      h('label.field', {}, h('span.field-label', {}, '기준 점 id'), idInput,
        h('span.field-help', {}, '훑어보기에서 돋보기를 누르면 여기 채워집니다')),
      h('label.field', {}, h('span.field-label', {}, '또는 벡터'), vecInput),
      h('label.field.field-narrow', {}, h('span.field-label', {}, '개수'), limitInput),
      feats.filter
        ? h('label.field', {}, h('span.field-label', {}, '필터 (JSON)'), filterInput,
          h('span.field-help', {}, '이 DB 의 필터 문법을 그대로 씁니다'))
        : null,
      h('button.btn.btn-primary', { type: 'button', onclick: run },
        icon('search', 14), '찾기'),
    ),
    results,
  );
  // 훑어보기에서 돋보기를 눌러 왔으면 바로 찾는다. 한 번 더 누르게 하면
  // "눌렀는데 아무 일도 안 났다"가 되고, 그때 사람은 단추를 다시 찾는다.
  if (picks.similar) {
    picks.similar = '';
    run();
  }
  return box;
}

function resultView(result, picks, goto) {
  const points = result.points ?? [];
  return h('div', {},
    h('div.card-title', {},
      h('span', {}, '결과'),
      h('span.muted.small', {}, `${points.length}개 · ${(result.elapsedMs ?? 0).toFixed(1)}ms`),
      result.metric ? badge(METRIC_LABELS[result.metric] ?? result.metric, 'neutral') : null,
    ),
    ...(result.notes ?? []).map((n) => h('p.notice.notice-info', {}, icon('alert'), n)),
    points.length === 0
      ? emptyState('가까운 벡터가 없습니다.')
      : h('div.vec-points', {}, points.map((p) => pointCard(p, picks, goto))),
  );
}

// ---------- 비교 ----------

function compareView(conn, col, picks) {
  const box = h('div.vec-panel');
  const aInput = input({ value: picks.a ?? '', placeholder: '왼쪽 점의 id' });
  const bInput = input({ value: picks.b ?? '', placeholder: '오른쪽 점의 id' });
  const out = h('div');

  async function run() {
    const a = aInput.value.trim();
    const b = bInput.value.trim();
    if (!a || !b) {
      toast('두 점의 id 를 모두 넣으세요', 'error');
      return;
    }
    mount(out, spinner('견주는 중…'));
    try {
      const res = await api.post(`/connections/${conn.id}/vector/compare`,
        { collection: col.name, a, b });
      mount(out, comparisonView(res.comparison ?? {}, a, b));
    } catch (err) {
      mount(out, errorPanel(err));
    }
  }

  mount(box,
    h('p.field-help', {},
      '훑어보기와 이웃 찾기의 A · B 단추로 담을 수 있습니다. '
      + '세 가지 거리를 모두 내는 이유는, "왜 이 둘이 가깝다고 나오지"의 답이 '
      + '코사인에는 안 보이고 유클리드에는 보이는 일이 흔하기 때문입니다.'),
    h('div.vec-form', {},
      h('label.field', {}, h('span.field-label', {}, 'A'), aInput),
      h('label.field', {}, h('span.field-label', {}, 'B'), bInput),
      h('button.btn.btn-primary', { type: 'button', onclick: run },
        icon('workflow', 14), '견주기'),
    ),
    out,
  );
  // 둘 다 담겼으면 바로 견준다.
  if (picks.a && picks.b) run();
  return box;
}

function comparisonView(cmp, a, b) {
  if (!cmp.dimensions) return emptyState('견줄 수 없습니다.');
  return h('div', {},
    h('div.card-title', {},
      h('span', {}, '견준 결과'),
      h('span.muted.small', {}, `${cmp.dimensions}차원`)),
    ...(cmp.notes ?? []).map((n) => h('p.notice.notice-warn', {}, icon('alert'), n)),
    h('div.vec-compare', {},
      metricTile('코사인 유사도', cmp.cosine, '1에 가까울수록 방향이 같습니다 (-1 ~ 1)'),
      metricTile('유클리드 거리', cmp.euclid, '0에 가까울수록 가깝습니다'),
      metricTile('내적', cmp.dot, '길이와 방향을 함께 반영합니다'),
      metricTile('A 의 길이', cmp.normA, '코사인만 보면 사라지는 정보입니다'),
      metricTile('B 의 길이', cmp.normB, '두 길이가 크게 다르면 코사인과 유클리드가 다른 말을 합니다'),
    ),
    h('h3.vec-sub', {}, '가장 크게 갈리는 차원',
      h('span.muted.small', {}, ' 어디서 달라지는가를 보는 유일한 창입니다')),
    h('table.table.vec-deltas', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '차원'), h('th', {}, a || 'A'), h('th', {}, b || 'B'),
        h('th', {}, '차이'), h('th', {}, ''))),
      h('tbody', {}, (cmp.topDeltas ?? []).map((d) => deltaRow(d, cmp.topDeltas)))),
  );
}

function metricTile(label, value, help) {
  return h('div.vec-tile', { title: help },
    h('span.vec-tile-label', {}, label),
    h('span.vec-tile-value', {}, Number(value ?? 0).toFixed(4)),
    h('span.vec-tile-help', {}, help),
  );
}

function deltaRow(d, all) {
  const max = Math.max(...all.map((x) => Math.abs(x.diff)), 1e-9);
  const width = (Math.abs(d.diff) / max) * 100;
  return h('tr', {},
    h('td', {}, h('span.mono', {}, String(d.index))),
    h('td', {}, d.a.toFixed(4)),
    h('td', {}, d.b.toFixed(4)),
    h('td', {}, h('span', { class: d.diff < 0 ? 'vec-neg' : '' }, d.diff.toFixed(4))),
    h('td', {}, h('div.vec-delta-bar', {},
      h('span', { style: `width:${width}%`, class: d.diff < 0 ? 'is-neg' : '' }))),
  );
}

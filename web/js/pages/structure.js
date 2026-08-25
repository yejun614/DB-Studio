// 구조: 연결된 DB의 현재(또는 특정 버전) 스키마를 ERD로 본다.
//
// ERD 설계 화면과 캔버스를 공유하지만(core/erdcanvas.js) 목적이 반대다.
// 저쪽은 "만들고 싶은 것"을 그리고 여기서는 "지금 있는 것"을 읽는다.
// 그래서 **스키마는 편집할 수 없다.** 편집할 수 있는 것은 읽는 사람의 정리뿐이다 —
// 카드를 어디에 놓을지, 어디에 메모를 붙일지, 무엇을 묶어 볼지.
//
// 그 정리는 커넥션마다 하나 있는 **구조 문서**에 있고, 같은 DB를 보는 사람들이 함께
// 고친다(0032). 그래서 이 화면은 ERD 편집기와 같은 방·같은 op·같은 채팅을 쓴다 —
// 편집은 소켓으로 즉시 나가고, 서버가 seq를 붙여 모두에게 되돌려준다.
//
// 과거 버전을 보는 중에는 방을 열지 않는다. 그 화면은 지금이 아니라 그때를 보는
// 곳이라 함께 고칠 것이 없다.
import { api } from '../core/api.js';
import { state, kindLabel } from '../core/store.js';
import {
  h, mount, icon, select, input, spinner, emptyState, pageHeader, badge, envBadge,
  toast, relativeTime, formatDate, openModal, confirmDialog,
} from '../core/ui.js';
import { ErdCanvas, newLocalID, tableDisplay } from '../core/erdcanvas.js';
import { navigate } from '../core/router.js';
import { serverDbPicker } from '../core/connpick.js';
import { panelResizeHandle, attachPanelResize } from '../core/panelresize.js';
import { errorPanel } from './users.js';
import { ErdSession } from '../core/erdsocket.js';
import { roomChatView, scrollChatToBottom } from '../core/roomchat.js';
// 구조 화면과 ERD 편집기는 같은 op를 주고받는다. 반영 규칙을 두 벌로 두면
// 언젠가 어긋나고, 그때 조용히 사라지는 것은 남의 편집이다.
import { applyLightOp } from './erdeditor.js';

// 메모와 그룹에 쓸 색. ERD 편집기와 같은 팔레트여야 두 화면이 같은 언어로 읽힌다.
const TINTS = [
  { value: '#eab308', label: '노랑', className: 'tint-yellow' },
  { value: '#3b82f6', label: '파랑', className: 'tint-blue' },
  { value: '#22c55e', label: '초록', className: 'tint-green' },
  { value: '#ec4899', label: '분홍', className: 'tint-pink' },
  { value: '#a1a1aa', label: '회색', className: 'tint-gray' },
];

export async function renderStructure(outlet, params, query) {
  mount(outlet, spinner('커넥션 목록을 불러오는 중…'));

  let conns;
  let servers;
  try {
    // 서버 → DB 두 단계로 고른다(데이터·스키마 화면과 같은 규칙).
    [conns, servers] = await Promise.all([
      api.get('/connections/'),
      api.get('/servers/'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  // 구조를 ERD로 볼 수 있는 것은 관계형 DB뿐이다. Mongo/Redis는 테이블·관계 개념이
  // 없어 카드와 선으로 그릴 것이 없다(그 둘은 Mongo·Redis 화면이 맡는다).
  const targets = conns.items.filter((i) => {
    if (!i.accessible) return false;
    const info = state.meta?.dbKinds?.find((k) => k.kind === i.connection.kind);
    return info?.capabilities?.migrate;
  });

  if (targets.length === 0) {
    mount(outlet,
      pageHeader('구조', '연결된 DB의 구조를 ERD로 봅니다'),
      emptyState('구조를 볼 수 있는 커넥션이 없습니다. 관계형 DB 커넥션과 모니터링 등급 이상의 권한이 필요합니다.',
        h('a.btn', { href: '/connections' }, 'DB 커넥션으로')));
    return;
  }

  const connID = query.get('conn') || targets[0].connection.id;
  const current = targets.find((i) => i.connection.id === connID) ?? targets[0];
  const conn = current.connection;
  const versionParam = query.get('version') ?? '';

  const view = new StructureView(outlet, targets, conn, versionParam, servers.items ?? []);
  await view.start();
  return () => view.stop();
}

class StructureView {
  constructor(outlet, targets, conn, versionParam, servers) {
    this.outlet = outlet;
    this.targets = targets;
    this.conn = conn;
    this.versionParam = versionParam;
    this.servers = servers ?? [];
    this.data = null;
    this.selection = null;
    // 실시간 방 상태. docID가 비어 있으면 과거 시점을 보는 중이다.
    this.docID = '';
    this.canEdit = false;
    this.session = null;
    this.participants = [];
    this.you = null;
    // 오른쪽 패널은 인스펙터와 대화가 번갈아 쓴다. 둘을 같이 띄우면 캔버스가 좁아지고,
    // 좁은 캔버스는 이 화면의 목적(전체를 훑어보기)을 정면으로 해친다.
    this.tab = 'inspect';
    this.chat = [];
    this.unread = 0;
    this.panelHidden = false;
  }

  async start() {
    this.build();
    await this.loadVersions();
    await this.load();
  }

  stop() {
    // 떠나기 전에 미뤄둔 저장을 반드시 보낸다. 여기서 흘리면 사람이 방금 정리한
    // 배치가 아무 표시 없이 사라지고, 다음에 열었을 때 원래대로 돌아가 있다.
    this.session?.close();
    this.session = null;
    this.canvas?.destroy();
    if (this.unbind) this.unbind();
  }

  build() {
    this.picker = serverDbPicker({
      usable: this.targets,
      servers: this.servers,
      currentId: this.conn.id,
      onPick: (id) => navigate(`/structure?conn=${encodeURIComponent(id)}`),
    });

    this.versionSelect = select([{ value: '', label: '현재 DB (최신)' }], { value: this.versionParam });
    this.versionSelect.addEventListener('change', () => {
      const v = this.versionSelect.value;
      navigate(`/structure?conn=${encodeURIComponent(this.conn.id)}`
        + (v ? `&version=${encodeURIComponent(v)}` : ''));
    });

    // 저장 표시가 아니라 연결 표시다. 편집은 op로 즉시 나가므로 "저장 대기"라는
    // 상태가 없다 — 대신 "지금 연결되어 있는가"가 사람이 알아야 할 사실이다.
    this.statusChip = h('span.erd-conn-status', {}, '연결 중…');
    this.participantsBox = h('div.erd-participants');
    this.sourceInfo = h('span.muted.small');
    this.canvasWrap = h('div.erd-canvas-wrap');
    this.panel = h('aside.erd-panel');
    this.panelResize = panelResizeHandle();
    this.toolbar = h('div.erd-toolbar');
    // 서버가 "왜 이만큼밖에 못 읽었는지"를 알려줄 자리.
    // 이것이 없으면 빈 캔버스가 접속 실패인지, 권한 문제인지, 정말 테이블이
    // 없는 것인지 구분할 수 없다 — 사람은 그 셋에 각각 다르게 대응해야 한다.
    this.dbNotes = h('div.structure-notes');

    this.root = h('div.erd-editor.structure-view', {},
      h('header.erd-head', {},
        h('div.erd-head-main', {},
          h('h1.erd-title', {}, '구조'),
          envBadge(this.conn.environment),
          h('span.muted', {}, `${this.conn.name} · ${kindLabel(this.conn.kind)}`),
          this.sourceInfo,
        ),
        h('div.erd-head-side', {}, this.participantsBox, this.statusChip),
      ),
      h('div.card.filter-bar', {},
        ...this.picker.nodes,
        h('label.field.field-inline', {}, h('span.field-label', {}, '시점'), this.versionSelect),
      ),
      this.dbNotes,
      this.toolbar,
      h('div.erd-body', {}, this.canvasWrap, this.panelResize, this.panel),
    );
    mount(this.outlet, this.root);
    attachPanelResize({
      root: this.root,
      handle: this.panelResize,
      storageKey: 'dbstudio.structure.panelWidth',
      onResize: () => this.canvas?.render(),
    });

    this.canvas = new ErdCanvas(this.canvasWrap, {
      emptyHint: '테이블이 없습니다',
      // 과거 시점을 보는 중이거나 등급이 없으면 손으로 옮길 수도 없어야 한다.
      // 옮겨지긴 하는데 저장되지 않으면, 새로고침 때 제자리로 돌아가는 이유를
      // 화면에서 알 수 없다.
      canEdit: () => this.canEdit,
      // 선택은 종류를 함께 갖는다. 메모·그룹도 인스펙터에서 고치므로
      // "무엇을 고른 상태인가"를 문자열 하나로는 표현할 수 없다.
      onSelect: (key) => this.select(key ? { kind: 'table', id: key } : null),
      onSelectNote: (id) => this.select({ kind: 'note', id }),
      onSelectGroup: (id) => this.select({ kind: 'group', id }),
      // 배치를 바꾸는 것들. 캔버스가 로컬 상태를 이미 갱신했으므로 같은 변경을
      // op로 보내기만 한다 — 서버가 seq를 붙여 모두에게 되돌려준다.
      onTableMove: (key, x, y) => this.op('table.move', { key, x, y }),
      onNoteMove: (id, x, y) => this.op('note.update', { id, x, y }),
      onNoteResize: (id, w, h) => this.op('note.update', { id, w, h }),
      onGroupMove: (id, x, y) => this.op('group.update', { id, x, y }),
      onGroupResize: (id, w, h) => this.op('group.update', { id, w, h }),
      onToggleCollapse: (key, geom) => this.op('table.move', {
        key, x: geom.x, y: geom.y, collapsed: !geom.layout.collapsed,
      }),
      // 커서는 편집이 아니라 존재의 표시다. 저장하지 않고 흘려보낸다.
      onCursorMove: (pt) => this.session?.presence({ cursor: pt }),
      // 두 번 눌러도 인스펙터로 간다. 편집기가 두 벌이면 한쪽만 고치게 된다.
      onEditNote: (note) => this.select({ kind: 'note', id: note.id }),
      onEditGroup: (group) => this.select({ kind: 'group', id: group.id }),
    });

    // 탭이 숨으면 소켓을 잠시 놓는다. 열어 둔 탭마다 참여자로 남으면 "지금 누가
    // 보고 있는가"가 뜻을 잃는다.
    const onHide = () => this.session?.suspend();
    const onShow = () => this.session?.resume();
    window.addEventListener('pagehide', onHide);
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'hidden') onHide(); else onShow();
    });
    this.unbind = () => window.removeEventListener('pagehide', onHide);
  }

  async loadVersions() {
    try {
      const res = await api.get(`/connections/${encodeURIComponent(this.conn.id)}/versions?limit=50`);
      const options = [{ value: '', label: '현재 DB (최신)' }].concat(
        (res.versions ?? []).map((v) => ({
          value: String(v.id),
          label: `버전 ${v.versionNo} — ${formatDate(v.createdAt)}`,
        })));
      mount(this.versionSelect, options.map((o) =>
        h('option', { value: o.value, selected: o.value === this.versionParam }, o.label)));
    } catch {
      // 버전 이력을 못 읽어도 현재 구조는 볼 수 있어야 한다.
      // 목록이 "현재 DB" 하나로 남을 뿐이다.
    }
  }

  async load() {
    // 로딩 표시는 캔버스를 지우지 않고 그 위에 덮는다. 캔버스를 비우면 그 안의
    // svg가 사라지고, 그러면 이미 리스너가 붙은 캔버스를 통째로 다시 만들어야 한다.
    const overlay = h('div.structure-overlay', {}, spinner('구조를 읽는 중…'));
    this.canvasWrap.appendChild(overlay);

    const url = `/connections/${encodeURIComponent(this.conn.id)}/structure`
      + (this.versionParam ? `?version=${encodeURIComponent(this.versionParam)}` : '');
    let res;
    try {
      res = await api.get(url);
    } catch (err) {
      mount(overlay, errorPanel(err));
      return;
    }
    overlay.remove();
    this.data = res;

    // 캔버스가 기대하는 모양(문서)으로 맞춘다. 스키마는 서버가 준 것 그대로이고,
    // 배치·메모·묶음은 구조 문서(함께 보는 것)의 것이다.
    this.doc = {
      schema: res.schema,
      layout: res.layout ?? {},
      notes: res.notes ?? [],
      groups: res.groups ?? [],
    };
    this.canEdit = Boolean(res.canEdit);

    this.selection = null;
    this.canvas.setSelection(null);
    this.canvas.setDoc(this.doc);
    this.canvas.fitView();
    this.canvas.render();
    this.renderToolbar();
    this.renderPanel();
    this.renderSource();
    this.renderDBNotes();
    // 실시간 방에 붙는다. 주소가 문서 하나를 가리키므로 커넥션을 바꾸면 방도 바뀐다.
    this.connect(res.documentId ?? '');
    this.loadDrafts();

    // 서버가 새 테이블에 자리를 잡아 줬으면 그 사실을 남긴다.
    // 저장해 두지 않으면 다음에 열 때 또 새로 배치되어, 옮겨 둔 것과 섞인다.
    if (res.placed > 0) {
      // 자리는 서버가 이미 문서에 넣어 두었다(구조 문서 갱신). 여기서는 알리기만 한다.
      toast(`새 테이블 ${res.placed}개를 배치했습니다`, 'info');
    }
  }

  // renderDBNotes는 스키마를 읽으면서 서버가 남긴 말을 보여준다.
  //
  // introspect는 부분 실패를 에러로 만들지 않는다 — 인덱스를 못 읽었다고 테이블까지
  // 못 보여줄 이유는 없기 때문이다. 대신 이유를 note로 남기는데, 그것을 화면이 버리면
  // 사용자에게는 그냥 "일부가 빠진 그림"으로 보인다. 특히 테이블이 하나도 없을 때는
  // 화면이 완전히 비어서, 읽기에 실패한 것과 구분할 방법이 아예 없다.
  renderDBNotes() {
    const notes = this.data?.notesFromDB ?? [];
    const empty = (this.doc?.schema?.tables ?? []).length === 0;
    if (notes.length === 0 && !empty) {
      mount(this.dbNotes);
      return;
    }

    const items = notes.slice();
    // 이유를 못 받았는데 비어 있다면 최소한 "비어 있다"는 사실만이라도 말한다.
    if (empty && items.length === 0) {
      items.push(this.data?.source?.kind === 'version'
        ? '이 버전에는 테이블이 없습니다.'
        : '이 커넥션에서 읽을 수 있는 테이블이 없습니다.');
    }
    if (empty && this.conn.kind === 'oracle') {
      // Oracle은 접속 계정의 스키마만 읽는다. 계정과 소유자가 다른 구성이 흔해서
      // (관리 계정으로 붙고 테이블은 앱 계정 소유) 여기서 막히는 일이 잦다.
      items.push('Oracle은 접속 계정이 소유한 스키마를 읽습니다. '
        + '테이블 소유자가 다른 계정이라면 커넥션 옵션의 "스키마 소유자"(owner)를 지정하세요.');
    }

    mount(this.dbNotes,
      h(`div.notice.${empty ? 'notice-warn' : 'notice-info'}`, {},
        icon('alert'),
        h('span', {},
          h('strong', {}, empty ? '구조를 그릴 것이 없습니다' : '스키마를 일부만 읽었습니다'),
          items.length === 1
            ? items[0]
            : h('ul.note-list', {}, items.map((n) => h('li', {}, n))),
        )));
  }

  renderSource() {
    const src = this.data?.source ?? {};
    const stats = this.data?.stats ?? {};
    mount(this.sourceInfo,
      src.kind === 'version'
        ? `버전 ${src.versionNo} · ${relativeTime(src.createdAt)}`
        : '현재 DB',
      ` · 테이블 ${stats.tables ?? 0}개`);
  }

  renderToolbar() {
    const readOnly = this.data?.source?.kind === 'version';
    mount(this.toolbar,
      // 메모·묶음은 함께 보는 것이라 편집 등급(erd)이 있어야 손댈 수 있다.
      // 없는 사람에게 버튼만 보여주면 눌러 본 뒤에야 알게 된다.
      this.canEdit && !readOnly
        ? h('div.erd-tool-group', {},
          h('button.btn.btn-small', { type: 'button', onclick: () => this.addNote() },
            icon('edit'), '메모'),
          h('button.btn.btn-small', { type: 'button', onclick: () => this.addGroup() },
            icon('list'), '그룹'),
        )
        : null,

      h('div.erd-tool-group', {},
        h('button.icon-btn', { type: 'button', title: '축소', onclick: () => this.canvas.zoom(1.25) }, '−'),
        h('button.icon-btn', { type: 'button', title: '확대', onclick: () => this.canvas.zoom(0.8) }, '+'),
        h('button.btn.btn-small', {
          type: 'button', title: '모든 테이블이 보이도록 확대/이동합니다',
          onclick: () => { this.canvas.fitView(); this.canvas.render(); },
        }, '화면에 맞추기'),
        h('button.btn.btn-small', {
          type: 'button', title: '카드를 격자에 다시 늘어놓습니다',
          onclick: () => this.relayout(),
        }, icon('refresh'), '자동 배치'),
      ),
      h('div.erd-tool-group', {},
        h('button.btn.btn-small', { type: 'button', onclick: () => this.load() },
          icon('refresh'), '새로고침'),
        // 구조에서 곧바로 설계를 시작하는 흐름. 지금 보고 있는 것을 그대로
        // 초안으로 옮기므로, ERD 화면에서 커넥션을 다시 고를 필요가 없다.
        // 작업 중인 초안으로 건너뛰는 길. 한 DB에 초안이 여럿일 수 있으므로
        // 고르개로 둔다 — 목록 화면을 거쳐 다시 찾아 들어갈 이유가 없다.
        this.drafts?.length ? this.draftPicker() : null,
        h('button.btn.btn-small', { type: 'button', onclick: () => this.createDraft() },
          icon('plus'), '이 구조로 초안 만들기'),
        // 버전 이력으로 돌아가는 길. 여기 오는 흔한 경로가 그 화면의 "구조 보기"이고,
        // 돌아갈 버튼이 없으면 브라우저 뒤로가기밖에 남지 않는다 — 그것은 시점을
        // 바꿔 본 뒤에는 엉뚱한 곳으로 간다(주소가 그만큼 쌓여 있다).
        //
        // 시점이 "현재"일 때도 두는 이유: 이 커넥션의 이력으로 가는 길은 어느
        // 상태에서든 쓸모가 있고, 상태에 따라 나타났다 사라지는 버튼은 찾을 때
        // 있는지부터 의심하게 만든다.
        h('a.btn.btn-small', {
          href: `/versions?conn=${encodeURIComponent(this.conn.id)}`,
          title: '이 DB의 스키마 버전 이력으로 갑니다',
        }, icon('history'), '버전 이력'),
      ),
      h('div.erd-tool-spacer'),
      readOnly
        ? h('span.muted.small', {}, '과거 시점을 보고 있습니다 — 정리는 현재 시점에서 고칩니다')
        : null,
      // 오른쪽 끝은 패널 탭이다. ERD 설계 화면과 같은 자리에 같은 순서로 둔다 —
      // 두 화면을 오가는 사람이 매번 찾지 않아도 되도록. (도메인 탭은 없다:
      // 도메인은 "만들고 싶은 것"을 그리는 초안의 개념이고, 구조 화면의 스키마는
      // 실제 DB에서 읽은 것이라 고칠 수 없다.)
      h('div.erd-tool-group', {},
        h('button.btn.btn-small', {
          type: 'button',
          class: this.tab === 'inspect' ? 'btn btn-small btn-active' : 'btn btn-small',
          onclick: () => { this.tab = 'inspect'; this.renderPanel(); },
        }, '속성'),
        h('button.btn.btn-small', {
          type: 'button',
          class: this.tab === 'chat' ? 'btn btn-small btn-active' : 'btn btn-small',
          onclick: () => { this.tab = 'chat'; this.renderPanel(); },
        }, '대화', this.unread > 0 ? badge(String(this.unread), 'info') : null),
        // 사이드바를 접으면 캔버스가 그만큼 넓어진다. 테이블이 스무 개를 넘어가면
        // 그 폭이 "한 화면에 다 보이는가"를 가른다.
        h('button.icon-btn.btn-tip', {
          type: 'button',
          'data-tip': this.panelHidden ? '사이드바 보이기' : '사이드바 숨기기',
          'aria-label': this.panelHidden ? '사이드바 보이기' : '사이드바 숨기기',
          onclick: () => this.togglePanel(),
        }, icon(this.panelHidden ? 'chevron-left' : 'chevron-right')),
      ),
    );
  }

  // ---------- 사이드 패널 ----------

  select(sel) {
    this.selection = sel;
    this.canvas.setSelection(sel);
    this.canvas.render();
    this.renderPanel();
  }

  note(id) {
    return (this.doc?.notes ?? []).find((n) => n.id === id) ?? null;
  }

  group(id) {
    return (this.doc?.groups ?? []).find((g) => g.id === id) ?? null;
  }

  renderPanel() {
    if (this.tab === 'chat') {
      this.unread = 0;
      mount(this.panel, this.chatView());
      scrollChatToBottom(this.panel);
      this.renderToolbar();
      return;
    }
    if (this.selection?.kind === 'note') {
      const note = this.note(this.selection.id);
      if (note) {
        mount(this.panel, this.noteView(note));
        return;
      }
      this.selection = null;
    }
    if (this.selection?.kind === 'group') {
      const group = this.group(this.selection.id);
      if (group) {
        mount(this.panel, this.groupView(group));
        return;
      }
      this.selection = null;
    }

    const tbl = this.selection?.kind === 'table'
      ? (this.doc?.schema?.tables ?? []).find((t) => keyOf(t) === this.selection.id)
      : null;

    if (!tbl) {
      mount(this.panel, h('div.erd-panel-empty', {},
        icon('database', 24),
        h('p', {}, '테이블을 선택하면 컬럼과 제약을 볼 수 있습니다.'),
        h('p.muted', {},
          '이 화면에서는 구조를 바꿀 수 없습니다. 카드를 끌어 옮기거나 메모·그룹을 '
          + '더할 수 있고, 그 정리는 내 계정에만 저장됩니다.'),
      ));
      return;
    }

    mount(this.panel,
      h('div.erd-panel-head', {},
        h('h2', {}, tableDisplay(tbl)),
        h('button.icon-btn', {
          type: 'button', title: '선택 해제',
          onclick: () => this.select(null),
        }, icon('x'))),
      h('div.erd-panel-body', {},
        tbl.comment ? h('p.muted', {}, tbl.comment) : null,
        h('dl.erd-card-stats', {},
          h('div', {}, h('dt', {}, '행(추정)'), h('dd', {}, formatCount(tbl.rowEstimate))),
          h('div', {}, h('dt', {}, '크기'), h('dd', {}, formatBytes(tbl.sizeBytes))),
        ),

        h('h3.erd-sub', {}, '컬럼', h('span.muted', {}, `${tbl.columns?.length ?? 0}개`)),
        h('div.structure-cols', {}, (tbl.columns ?? []).map((col) => {
          const isPK = (tbl.primaryKey?.columns ?? [])
            .some((c) => c.toLowerCase() === col.name.toLowerCase());
          const isFK = (tbl.foreignKeys ?? []).some((fk) =>
            (fk.columns ?? []).some((c) => c.toLowerCase() === col.name.toLowerCase()));
          return h('div.structure-col', {},
            h('span.structure-col-mark', {}, isPK ? icon('key', 12) : isFK ? '◆' : ''),
            h('span.structure-col-name', {}, col.name),
            h('span.structure-col-type', {}, col.rawType || col.type?.base || ''),
            col.nullable ? null : badge('NOT NULL', 'neutral'),
            col.identity ? badge('AUTO', 'info') : null,
            col.comment ? h('p.erd-col-comment', {}, col.comment) : null,
          );
        })),

        h('h3.erd-sub', {}, '인덱스', h('span.muted', {}, `${tbl.indexes?.length ?? 0}개`)),
        h('div.erd-chip-list', {}, (tbl.indexes ?? []).length === 0
          ? h('p.muted', {}, '없음')
          : (tbl.indexes ?? []).map((idx) => h('div.erd-chip', {},
            h('span', {}, `${idx.unique ? 'UNIQUE ' : ''}${idx.name}`),
            h('span.muted', {}, (idx.columns ?? []).map((c) => c.column || c.expression).join(', '))))),

        h('h3.erd-sub', {}, '외래키', h('span.muted', {}, `${tbl.foreignKeys?.length ?? 0}개`)),
        h('div.erd-chip-list', {}, (tbl.foreignKeys ?? []).length === 0
          ? h('p.muted', {}, '없음')
          : (tbl.foreignKeys ?? []).map((fk) => h('div.erd-chip', {},
            h('span', {}, fk.name),
            h('span.muted', {},
              `${(fk.columns ?? []).join(', ')} → ${fk.refTable}(${(fk.refColumns ?? []).join(', ')})`),
            fk.onDelete ? badge(`ON DELETE ${fk.onDelete}`, 'neutral') : null))),

        (tbl.checks ?? []).length
          ? [h('h3.erd-sub', {}, '체크 제약'),
            h('div.erd-chip-list', {}, tbl.checks.map((ck) => h('div.erd-chip', {},
              h('span', {}, ck.name), h('code.muted', {}, ck.expression))))]
          : null,
      ));
  }

  // ---------- 배치 편집 ----------

  addNote() {
    const box = h('textarea.input.textarea', { rows: 3, placeholder: '메모 내용' });
    openModal({
      title: '메모 추가',
      width: 460,
      body: () => [
        box,
        h('p.field-help', {}, '이 메모는 이 DB를 보는 모두에게 보입니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const at = this.canvas.center(100, 40);
            const note = { id: newLocalID('n'), text: box.value, ...at, color: '' };
            this.doc.notes.push(note);
            this.canvas.render();
            this.op('note.add', note);
            close();
          },
        }, '추가'),
      ],
    });
  }

  // noteView는 메모를 인스펙터에서 고친다(모달 대신).
  //
  // 편집기가 두 벌(모달·인스펙터)이면 한쪽만 고치게 되고, 사용자는 어느 쪽이
  // 진짜인지 알 수 없다. ERD 설계 화면과 같은 자리에 둔다.
  noteView(note) {
    const box = h('textarea.input.textarea', { rows: 5, value: note.text ?? '' });
    const commit = () => {
      if (note.text === box.value) return;
      note.text = box.value;
      this.canvas.render();
      this.op('note.update', { id: note.id, text: note.text });
    };
    box.addEventListener('blur', commit);

    return [
      h('div.erd-panel-head', {},
        h('h2', {}, '메모'),
        h('button.icon-btn', { type: 'button', title: '선택 해제', onclick: () => this.select(null) },
          icon('x'))),
      h('div.erd-panel-body', {},
        h('label.field', {}, h('span.field-label', {}, '내용'), box,
          h('span.field-help', {}, '입력을 마치고 다른 곳을 누르면 저장됩니다')),
        h('div.field', {}, h('span.field-label', {}, '색'),
          this.tintPicker(note.color ?? '', (value) => {
            note.color = value;
            this.canvas.render();
            this.op('note.update', { id: note.id, color: value });
            this.renderPanel();
          })),
        h('p.field-help', {}, '크기는 캔버스에서 오른쪽 아래 모서리를 끌어 정합니다.'),
        h('button.btn.btn-danger-ghost.btn-block', {
          type: 'button',
          onclick: () => {
            this.doc.notes = this.doc.notes.filter((n) => n.id !== note.id);
            this.canvas.setDoc(this.doc);
            this.select(null);
            this.op('note.delete', { id: note.id });
          },
        }, icon('trash'), '메모 삭제'),
      ),
    ];
  }

  // groupView는 그룹을 인스펙터에서 고친다.
  groupView(group) {
    const nameInput = input({ value: group.label ?? '' });
    const commit = () => {
      const next = nameInput.value.trim();
      if (next === (group.label ?? '')) return;
      group.label = next;
      this.canvas.render();
      this.op('group.update', { id: group.id, label: next });
    };
    nameInput.addEventListener('blur', commit);
    nameInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        commit();
      }
    });

    return [
      h('div.erd-panel-head', {},
        h('h2', {}, '그룹'),
        h('button.icon-btn', { type: 'button', title: '선택 해제', onclick: () => this.select(null) },
          icon('x'))),
      h('div.erd-panel-body', {},
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('div.field', {}, h('span.field-label', {}, '색'),
          this.tintPicker(group.color ?? '', (value) => {
            group.color = value;
            this.canvas.render();
            this.op('group.update', { id: group.id, color: value });
            this.renderPanel();
          })),
        h('p.field-help', {}, '크기와 위치는 캔버스에서 끌어 정합니다. 이 정리는 함께 봅니다.'),
        h('button.btn.btn-danger-ghost.btn-block', {
          type: 'button',
          onclick: () => {
            this.doc.groups = this.doc.groups.filter((g) => g.id !== group.id);
            this.canvas.setDoc(this.doc);
            this.select(null);
            this.op('group.delete', { id: group.id });
          },
        }, icon('trash'), '그룹 삭제'),
      ),
    ];
  }

  // tintPicker는 색 고르개다. 메모와 그룹이 같은 것을 쓴다.
  tintPicker(current, onPick) {
    return h('div.tint-picker', {}, TINTS.map((c) => h('button.tint-swatch', {
      type: 'button',
      class: `${c.className}${(current || '') === c.value ? ' is-on' : ''}`,
      title: c.label,
      onclick: () => onPick(c.value),
    })));
  }

  addGroup() {
    const nameInput = input({ placeholder: '예: 주문 도메인' });
    let color = TINTS[1].value;
    const swatches = h('div.tint-picker', {}, TINTS.map((c) => h('button.tint-swatch', {
      type: 'button',
      class: `${c.className}${c.value === color ? ' is-on' : ''}`,
      title: c.label,
      onclick: (e) => {
        color = c.value;
        for (const b of e.currentTarget.parentElement.children) b.classList.remove('is-on');
        e.currentTarget.classList.add('is-on');
      },
    })));

    openModal({
      title: '그룹 추가',
      width: 460,
      body: () => [
        h('label.field', {}, h('span.field-label', {}, '이름'), nameInput),
        h('div.field', {}, h('span.field-label', {}, '색'), swatches),
        h('p.field-help', {},
          '테이블을 감싸는 사각형입니다. 크기와 위치는 캔버스에서 끌어 정합니다. '
          + '내 계정에만 저장됩니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const at = this.canvas.center(160, 120);
            const group = {
              id: newLocalID('g'), label: nameInput.value.trim(), ...at,
              w: 320, h: 240, color,
            };
            this.doc.groups.push(group);
            this.canvas.render();
            this.op('group.add', group);
            close();
          },
        }, '추가'),
      ],
    });
  }

  // relayout은 카드를 격자에 다시 늘어놓는다.
  // 테이블이 많이 늘어나 배치가 엉킨 뒤에 처음부터 다시 시작하는 길이다.
  async relayout() {
    const ok = await confirmDialog({
      title: '자동 배치',
      message: '카드를 격자에 다시 늘어놓습니다. 직접 옮겨 둔 위치는 사라집니다. '
        + '메모와 그룹은 그대로 남습니다.',
      confirmLabel: '다시 배치',
    });
    if (!ok) return;
    const tables = this.doc.schema?.tables ?? [];
    const next = {};
    tables.forEach((t, i) => {
      const key = keyOf(t);
      // 서버(erd.AutoLayout)와 같은 격자다. 두 화면의 초기 배치가 같아야
      // 설계 화면과 오갈 때 같은 그림으로 읽힌다.
      next[key] = {
        ...(this.doc.layout[key] ?? {}),
        x: 80 + (i % 4) * 320,
        y: 80 + Math.floor(i / 4) * 260,
      };
    });
    this.doc.layout = next;
    this.canvas.setDoc(this.doc);
    this.canvas.fitView();
    this.canvas.render();
    // 카드마다 op 하나. 좌표 대입이라 순서가 뒤바뀌어도 결과가 같고, 함께 보는
    // 사람의 화면도 같은 자리로 움직인다.
    for (const [key, box] of Object.entries(next)) {
      this.op('table.move', { key, x: box.x, y: box.y });
    }
  }

  // loadDrafts는 이 커넥션의 ERD 초안 목록을 받아 둔다.
  //
  // 실패해도 조용히 넘어간다. 초안 바로가기는 편의이고, 그것 때문에 구조가 안 보이면
  // 잃는 것이 더 크다.
  async loadDrafts() {
    try {
      const res = await api.get('/erd/documents/');
      // 목록은 {items: [{document, connection, ...}]} 모양이다.
      this.drafts = (res.items ?? [])
        .map((it) => it.document)
        .filter((d) => d && d.connectionId === this.conn.id)
        .sort((x, y) => String(y.updatedAt).localeCompare(String(x.updatedAt)));
    } catch {
      this.drafts = [];
    }
    this.renderToolbar();
  }

  // draftPicker는 이 DB의 초안 중 하나로 건너뛰는 고르개다.
  draftPicker() {
    const picker = select([
      { value: '', label: `ERD 초안 (${this.drafts.length})` },
      ...this.drafts.map((d) => ({
        value: d.id,
        label: `${d.name}${d.status && d.status !== 'draft' ? ` · ${draftStatusLabel(d.status)}` : ''}`,
      })),
    ], { value: '' });
    picker.classList.add('structure-draft-pick');
    picker.title = '이 DB로 작업 중인 ERD 초안으로 갑니다';
    picker.addEventListener('change', () => {
      if (!picker.value) return;
      navigate(`/erd/${encodeURIComponent(picker.value)}`);
    });
    return picker;
  }

  // createDraft는 지금 보고 있는 커넥션으로 ERD 초안을 만든다.
  createDraft() {
    const nameInput = input({
      value: `${this.conn.name} 구조 사본`, autofocus: true,
    });
    openModal({
      title: '이 구조로 초안 만들기',
      width: 520,
      body: () => [
        h('p.modal-message', {},
          '이 커넥션의 현재 스키마를 가져와 편집 가능한 ERD 초안을 만듭니다. '
          + '초안에서 고친 것은 마이그레이션을 만들기 전까지 실제 DB에 영향을 주지 않습니다.'),
        h('label.field', {}, h('span.field-label', {}, '문서 이름'), nameInput),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: async (e) => {
            const btn = e.currentTarget;
            btn.disabled = true;
            try {
              const res = await api.post('/erd/documents/', {
                name: nameInput.value,
                connectionId: this.conn.id,
                fromConnection: true,
              });
              close();
              navigate(`/erd/${encodeURIComponent(res.document.id)}`);
            } catch (err) {
              btn.disabled = false;
              toast(err.message ?? '만들지 못했습니다', 'error', 6000);
            }
          },
        }, '만들기'),
      ],
    });
  }

  togglePanel() {
    this.panelHidden = !this.panelHidden;
    this.root.classList.toggle('is-panel-hidden', this.panelHidden);
    this.renderToolbar();
    // 캔버스 폭이 바뀌었으므로 좌표계를 다시 잡는다.
    this.canvas.render();
  }

  // ---------- 실시간 ----------

  // op는 편집 하나를 방에 보낸다.
  //
  // 로컬 반영은 호출부가 이미 했다. 낙관적으로 먼저 그리지 않으면 왕복 지연 때문에
  // 드래그와 타이핑이 끊기기 때문이다 — 서버는 같은 op에 seq를 붙여 되돌려주고,
  // 거부하면 문서 전체를 함께 보내 화면을 바로잡는다.
  op(kind, payload) {
    if (!this.session || !this.canEdit) return null;
    return this.session.send(kind, payload);
  }

  // connect는 이 커넥션의 구조 문서 방에 붙는다.
  //
  // 과거 버전을 보는 중에는 방이 없다(documentId가 비어 온다). 그 화면은 지금이
  // 아니라 그때를 보는 곳이라 함께 고칠 것이 없다.
  connect(docID) {
    if (this.session && this.docID === docID) return;
    this.session?.close();
    this.session = null;
    this.docID = docID;
    if (!docID) {
      this.setStatus('none');
      return;
    }
    this.session = new ErdSession(docID, {
      onStatus: (state) => {
        this.setStatus(state);
        // 붙을 때마다 지금 보는 시점을 알린다. 재접속 뒤에도 참여자 목록의
        // "어느 시점을 보는가"가 맞아야 커서 필터가 뜻을 갖는다.
        if (state === 'connected') this.session?.presence({ view: this.viewKey() });
      },
      onInit: (msg) => this.onDocument(msg),
      onState: (msg) => this.onDocument(msg),
      onOp: (op, mine, hasDoc) => this.onOp(op, mine, hasDoc),
      onReject: (msg) => this.onReject(msg),
      onPresence: (list) => this.onPresence(list),
      onCursor: (msg) => this.onCursor(msg),
      onChat: (m) => this.onChat(m),
      onError: (message) => {
        toast(message, 'error');
        // 거부된 편집의 미리보기가 화면에 남지 않게 문서를 다시 받는다.
        this.session?.resync(true);
      },
    });
    this.session.connect();
  }

  setStatus(state) {
    const map = {
      connected: [this.versionParam ? '실시간 연결됨 · 과거 시점' : '실시간 연결됨', 'is-ok'],
      reconnecting: ['다시 연결하는 중…', 'is-warn'],
      disconnected: ['연결 끊김', 'is-bad'],
      error: ['연결 오류', 'is-bad'],
      none: ['연결 없음', ''],
    };
    const [label, cls] = map[state] ?? ['연결 중…', ''];
    mount(this.statusChip, label);
    this.statusChip.className = `erd-conn-status ${cls}`.trim();
  }

  // onDocument는 서버가 보낸 문서 전체로 화면을 맞춘다(접속·재동기화·거부 뒤).
  onDocument(msg) {
    if (!msg.document) return;
    const doc = msg.document;
    this.doc = {
      schema: doc.schema,
      layout: doc.layout ?? {},
      notes: doc.notes ?? [],
      groups: doc.groups ?? [],
    };
    if (msg.you) this.you = msg.you;
    // 첫 메시지에 참여자 목록이 함께 온다. 여기서 받지 않으면 다음 변화가 있을
    // 때까지 "0명 참여 중"으로 남아, 아무도 없는 방처럼 보인다.
    if (msg.participants) this.onPresence(msg.participants);
    if (msg.chat) {
      this.chat = msg.chat;
      if (this.tab === 'chat') this.renderPanel();
    }
    this.canEdit = Boolean(msg.you?.canEdit ?? this.canEdit);
    this.pruneSelection();
    this.canvas.setDoc(this.doc);
    this.canvas.render();
    this.renderPanel();
  }

  onOp(op, mine, hasDoc) {
    if (hasDoc) return; // 문서가 함께 왔으므로 이미 반영됐다
    applyLightOp(this.doc, op);
    this.pruneSelection();
    this.canvas.setDoc(this.doc);
    this.canvas.render();
    // 지금 인스펙터에 떠 있는 대상이 바뀌었으면 패널도 다시 그린다.
    if (this.touchesSelection(op)) this.renderPanel();
  }

  onReject(msg) {
    toast(msg.reason ?? '편집이 거부되었습니다', 'error');
    if (msg.document) this.onDocument(msg);
  }

  // touchesSelection은 이 op가 지금 고른 대상을 건드리는지 본다.
  // 남이 저쪽 끝에서 카드를 옮길 때마다 패널을 다시 그리면 입력하던 칸이 튄다.
  touchesSelection(op) {
    if (!this.selection) return false;
    const p = op.payload ?? {};
    switch (this.selection.kind) {
      case 'table': return op.kind === 'table.move' && p.key === this.selection.id;
      case 'note': return op.kind.startsWith('note.') && p.id === this.selection.id;
      case 'group': return op.kind.startsWith('group.') && p.id === this.selection.id;
      default: return false;
    }
  }

  // pruneSelection은 사라진 대상을 고른 상태를 풀어 준다.
  // 없는 메모를 편집하는 패널이 남아 있으면 그 뒤의 편집이 줄줄이 거부된다.
  pruneSelection() {
    const sel = this.selection;
    if (!sel) return;
    if (sel.kind === 'note' && !this.note(sel.id)) this.selection = null;
    if (sel.kind === 'group' && !this.group(sel.id)) this.selection = null;
    this.canvas.setSelection(this.selection);
  }

  onPresence(list) {
    this.participants = list ?? [];
    this.canvas.setParticipants(this.participants, this.you?.clientId);
    // 다른 시점으로 옮겨 간 사람의 커서는 지운다. 남겨 두면 유령이 붙어 있는다.
    for (const p of this.participants) {
      if ((p.view ?? '') !== this.viewKey()) this.canvas.setCursor(p.clientId, null);
    }
    this.renderParticipants();
    this.canvas.renderCursors();
    // 대화를 보고 있으면 "N명 참여 중"도 함께 맞춘다. 그러지 않으면 사람이 들어와도
    // 그 줄만 옛 숫자로 남아, 아무도 없는 방처럼 보인다.
    if (this.tab === 'chat') this.renderPanel();
  }

  // viewKey는 지금 보고 있는 시점이다(현재면 빈 문자열).
  viewKey() {
    return this.versionParam || '';
  }

  // viewLabel은 참여자 목록에 적을 시점 이름이다.
  viewLabel(view) {
    if (!view) return '현재';
    const opt = [...this.versionSelect.options].find((o) => o.value === String(view));
    return opt ? opt.textContent.split(' — ')[0] : '버전 ' + view;
  }

  onCursor(msg) {
    if (!msg.cursor || msg.clientId === this.you?.clientId) return;
    // 다른 시점을 보는 사람의 커서는 그리지 않는다. 그 좌표는 지금 화면에 없는
    // 테이블 위를 가리키므로, 보이면 "왜 저기를 가리키지"만 남는다.
    if ((msg.view ?? '') !== this.viewKey()) return;
    const who = this.participants.find((x) => x.clientId === msg.clientId);
    this.canvas.setCursor(msg.clientId, {
      x: msg.cursor.x, y: msg.cursor.y,
      color: who?.color ?? '#888', name: who?.userName ?? '',
    });
  }

  renderParticipants() {
    const others = this.participants.filter((p) => p.clientId !== this.you?.clientId);
    mount(this.participantsBox, others.slice(0, 6).map((p) => {
      const sameView = (p.view ?? '') === this.viewKey();
      return h('span.erd-avatar', {
        // 다른 시점을 보는 사람은 흐리게. 커서가 안 보이는 이유가 여기서 읽힌다.
        class: sameView ? 'erd-avatar' : 'erd-avatar is-elsewhere',
        style: { background: p.color ?? '#888' },
        'data-tip': p.userName + ' · ' + this.viewLabel(p.view)
          + (p.canEdit ? '' : ' (읽기 전용)'),
      }, (p.userName ?? '?').slice(0, 1));
    }));
  }

  // ---------- 대화 ----------

  onChat(message) {
    this.chat.push(message);
    if (this.tab === 'chat') {
      this.renderPanel();
      return;
    }
    // 대화를 보고 있지 않으면 읽지 않은 수만 표시한다. 탭을 강제로 열면
    // 지금 보고 있던 테이블이 화면에서 사라진다.
    this.unread += 1;
    this.renderToolbar();
  }

  chatView() {
    // 패널의 직계 자식으로 둔다(.erd-panel-body 로 싸지 않는다). 목록이 flex: 1 로
    // 남는 높이를 먹고 입력칸이 아래에 붙는 구조가 ERD 설계 화면과 같아야 한다.
    return [
      h('div.erd-panel-head', {},
        h('h2', {}, '대화'),
        h('button.icon-btn', {
          type: 'button', title: '속성으로', onclick: () => { this.tab = 'inspect'; this.renderPanel(); },
        }, icon('x'))),
      ...roomChatView({
        messages: this.chat,
        participants: this.participants.length,
        placeholder: '이 DB를 보는 사람들에게 남기기',
        emptyText: '아직 대화가 없습니다. 이 구조를 함께 보는 사람들에게 남겨 보세요.',
        onSend: (body) => this.session?.chat(body),
        // 방은 DB 기준이라 과거 시점에서도 대화는 된다. 방이 아예 없는 경우
        // (문서를 열지 못한 상태)에만 입력칸을 내린다.
        disabledNote: this.session ? '' : '연결되지 않아 대화를 보낼 수 없습니다.',
      }),
    ];
  }

}

// draftStatusLabel은 초안 상태를 고르개에 짧게 적는다.
function draftStatusLabel(status) {
  const map = { in_review: '리뷰 중', applied: '적용됨', archived: '보관' };
  return map[status] ?? status;
}

function keyOf(tbl) {
  return (tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name).toLowerCase();
}

function formatCount(n) {
  if (!n) return '—';
  return Number(n).toLocaleString();
}

function formatBytes(n) {
  if (!n) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = Number(n);
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)}${units[i]}`;
}

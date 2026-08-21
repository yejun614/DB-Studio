// 구조: 연결된 DB의 현재(또는 특정 버전) 스키마를 ERD로 본다.
//
// ERD 설계 화면과 캔버스를 공유하지만(core/erdcanvas.js) 목적이 반대다.
// 저쪽은 "만들고 싶은 것"을 그리고 여기서는 "지금 있는 것"을 읽는다.
// 그래서 **스키마는 편집할 수 없다.** 편집할 수 있는 것은 읽는 사람의 정리뿐이다 —
// 카드를 어디에 놓을지, 어디에 메모를 붙일지, 무엇을 묶어 볼지.
//
// 그 정리는 계정별로 저장된다. 결제를 보는 사람과 배송을 보는 사람은 같은 스키마를
// 다르게 늘어놓아야 하고, 한 사람의 정리가 다른 사람의 것을 덮어쓰면 둘 다 못 쓴다.
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

// 배치를 저장하기까지 기다리는 시간.
// 카드를 옮길 때마다 저장하면 드래그 한 번에 요청이 수십 개 나간다. 반대로 너무
// 길면 탭을 닫았을 때 방금 한 정리를 잃는다.
const SAVE_DELAY = 1200;

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
    this.saveTimer = null;
    this.dirty = false;
  }

  async start() {
    this.build();
    await this.loadVersions();
    await this.load();
  }

  stop() {
    // 떠나기 전에 미뤄둔 저장을 반드시 보낸다. 여기서 흘리면 사람이 방금 정리한
    // 배치가 아무 표시 없이 사라지고, 다음에 열었을 때 원래대로 돌아가 있다.
    this.flushSave();
    this.canvas?.destroy();
    if (this.unbind) this.unbind();
  }

  build() {
    this.picker = serverDbPicker({
      usable: this.targets,
      servers: this.servers,
      currentId: this.conn.id,
      onPick: (id) => {
        this.flushSave();
        navigate(`/structure?conn=${encodeURIComponent(id)}`);
      },
    });

    this.versionSelect = select([{ value: '', label: '현재 DB (최신)' }], { value: this.versionParam });
    this.versionSelect.addEventListener('change', () => {
      this.flushSave();
      const v = this.versionSelect.value;
      navigate(`/structure?conn=${encodeURIComponent(this.conn.id)}`
        + (v ? `&version=${encodeURIComponent(v)}` : ''));
    });

    this.saveChip = h('span.structure-save');
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
        h('div.erd-head-side', {}, this.saveChip),
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
      // 선택은 종류를 함께 갖는다. 메모·그룹도 인스펙터에서 고치므로
      // "무엇을 고른 상태인가"를 문자열 하나로는 표현할 수 없다.
      onSelect: (key) => this.select(key ? { kind: 'table', id: key } : null),
      onSelectNote: (id) => this.select({ kind: 'note', id }),
      onSelectGroup: (id) => this.select({ kind: 'group', id }),
      // 배치를 바꾸는 세 가지. 캔버스가 이미 로컬 상태를 갱신했으므로
      // 여기서는 "저장해야 한다"고 표시만 한다.
      onTableMove: () => this.markDirty(),
      onNoteMove: () => this.markDirty(),
      onNoteResize: () => this.markDirty(),
      onGroupMove: () => this.markDirty(),
      onGroupResize: () => this.markDirty(),
      onToggleCollapse: (key) => this.toggleCollapse(key),
      // 두 번 눌러도 인스펙터로 간다. 편집기가 두 벌이면 한쪽만 고치게 된다.
      onEditNote: (note) => this.select({ kind: 'note', id: note.id }),
      onEditGroup: (group) => this.select({ kind: 'group', id: group.id }),
    });

    // 탭을 닫거나 새로고침할 때도 미뤄둔 저장을 보낸다.
    const onHide = () => this.flushSave();
    window.addEventListener('pagehide', onHide);
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
    // 배치·메모·그룹은 이 사람의 것이다.
    this.doc = {
      schema: res.schema,
      layout: res.layout ?? {},
      notes: res.notes ?? [],
      groups: res.groups ?? [],
    };

    this.selection = null;
    this.canvas.setSelection(null);
    this.canvas.setDoc(this.doc);
    this.canvas.fitView();
    this.canvas.render();
    this.renderToolbar();
    this.renderPanel();
    this.renderSource();
    this.renderDBNotes();

    // 서버가 새 테이블에 자리를 잡아 줬으면 그 사실을 남긴다.
    // 저장해 두지 않으면 다음에 열 때 또 새로 배치되어, 옮겨 둔 것과 섞인다.
    if (res.placed > 0) {
      this.markDirty();
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
      h('div.erd-tool-group', {},
        h('button.btn.btn-small', { type: 'button', onclick: () => this.addNote() },
          icon('edit'), '메모'),
        h('button.btn.btn-small', { type: 'button', onclick: () => this.addGroup() },
          icon('list'), '그룹'),
      ),
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
        h('button.btn.btn-small', { type: 'button', onclick: () => this.createDraft() },
          icon('plus'), '이 구조로 초안 만들기'),
      ),
      h('div.erd-tool-spacer'),
      readOnly
        ? h('span.muted.small', {}, '과거 버전을 보고 있습니다 — 배치는 이 커넥션 공통으로 저장됩니다')
        : null,
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

  toggleCollapse(key) {
    const box = this.doc.layout[key] ?? { x: 80, y: 80 };
    this.doc.layout[key] = { ...box, collapsed: !box.collapsed };
    this.canvas.render();
    this.markDirty();
  }

  addNote() {
    const box = h('textarea.input.textarea', { rows: 3, placeholder: '메모 내용' });
    openModal({
      title: '메모 추가',
      width: 460,
      body: () => [
        box,
        h('p.field-help', {}, '이 메모는 내 계정에만 저장됩니다. 다른 사람에게는 보이지 않습니다.'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            const at = this.canvas.center(100, 40);
            this.doc.notes.push({ id: newLocalID('n'), text: box.value, ...at, color: '' });
            this.canvas.render();
            this.markDirty();
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
      this.markDirty();
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
            this.markDirty();
            this.renderPanel();
          })),
        h('p.field-help', {}, '크기는 캔버스에서 오른쪽 아래 모서리를 끌어 정합니다.'),
        h('button.btn.btn-danger-ghost.btn-block', {
          type: 'button',
          onclick: () => {
            this.doc.notes = this.doc.notes.filter((n) => n.id !== note.id);
            this.canvas.setDoc(this.doc);
            this.select(null);
            this.markDirty();
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
      this.markDirty();
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
            this.markDirty();
            this.renderPanel();
          })),
        h('p.field-help', {}, '크기와 위치는 캔버스에서 끌어 정합니다. 정리는 내 계정에만 저장됩니다.'),
        h('button.btn.btn-danger-ghost.btn-block', {
          type: 'button',
          onclick: () => {
            this.doc.groups = this.doc.groups.filter((g) => g.id !== group.id);
            this.canvas.setDoc(this.doc);
            this.select(null);
            this.markDirty();
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
            this.doc.groups.push({
              id: newLocalID('g'), label: nameInput.value.trim(), ...at,
              w: 320, h: 240, color,
            });
            this.canvas.render();
            this.markDirty();
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
    this.markDirty();
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

  // ---------- 저장 ----------

  markDirty() {
    this.dirty = true;
    mount(this.saveChip, '저장 대기 중…');
    this.saveChip.className = 'structure-save is-pending';
    clearTimeout(this.saveTimer);
    this.saveTimer = setTimeout(() => this.save(), SAVE_DELAY);
  }

  // flushSave는 미뤄둔 저장을 지금 보낸다.
  //
  // 화면을 떠날 때 부르므로 응답을 기다리지 않는다. 기다리면 페이지 전환이
  // 그만큼 멈추고, 실패해도 그때는 알릴 화면이 이미 없다.
  flushSave() {
    if (!this.dirty) return;
    clearTimeout(this.saveTimer);
    this.save();
  }

  async save() {
    if (!this.doc) return;
    this.dirty = false;
    const body = {
      layout: this.doc.layout ?? {},
      notes: this.doc.notes ?? [],
      groups: this.doc.groups ?? [],
    };
    try {
      await api.put(`/connections/${encodeURIComponent(this.conn.id)}/structure/view`, body);
      // 저장하는 동안 또 옮겼으면 다시 저장 대기 상태다. 여기서 "저장됨"으로
      // 덮어쓰면 아직 저장되지 않은 것을 저장됐다고 말하게 된다.
      if (this.dirty) return;
      mount(this.saveChip, '저장됨');
      this.saveChip.className = 'structure-save is-ok';
    } catch (err) {
      this.dirty = true;
      mount(this.saveChip, '저장 실패');
      this.saveChip.className = 'structure-save is-bad';
      toast(`배치를 저장하지 못했습니다: ${err.message}`, 'error');
    }
  }
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

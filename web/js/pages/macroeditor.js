// 매크로 노드 에디터.
//
// ERD 편집기와 달리 실시간 동기화가 없다(요구사항). 대신 저장이 곧 버전이므로,
// 두 사람이 같은 매크로를 고쳐도 나중 저장이 앞선 것을 지우지 않고 새 버전으로 쌓인다.
// 그래서 이 화면의 상태 관리는 단순하다: 로컬에서 그래프를 고치고, 저장을 누르면
// 통째로 새 버전이 된다.
//
// 렌더링은 ERD 편집기와 같은 판단을 따른다 — 구조가 바뀌면 캔버스를 다시 그린다.
// 예외는 드래그 중 좌표뿐이다(왕복이 없어도 재생성 비용이 눈에 띈다).
import { api } from '../core/api.js';
import {
  h, mount, icon, input, textarea, select, checkbox, spinner, badge,
  toast, toastError, openModal, confirmDialog, field, formatDate, relativeTime,
} from '../core/ui.js';
import { navigate, setLeaveGuard } from '../core/router.js';
import { codeEditor } from '../core/highlight.js';
import { errorPanel } from './users.js';
import { openRunLog, runStatusBadge } from './macros.js';
import { openShareDialog, visibilityBadge } from './macroshare.js';
import { triggerPanel } from './triggers.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

// 노드 상자 치수. 포트 위치 계산이 이 값들에 의존하므로 한곳에 모은다.
const NODE_W = 190;
const HEAD_H = 30;
const PORT_ROW = 20;
const BODY_PAD = 8;

export async function renderMacroEditor(outlet, params, query) {
  const macroID = params.id;
  mount(outlet, spinner('매크로를 여는 중…'));

  let data;
  let meta;
  let conns;
  let macros;
  try {
    const version = query.get('version');
    [data, meta, conns, macros] = await Promise.all([
      api.get(`/macros/${encodeURIComponent(macroID)}${version ? `?version=${version}` : ''}`),
      api.get('/macros/meta'),
      api.get('/connections/'),
      api.get('/macros/'),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const editor = new MacroEditor(outlet, macroID, data, meta, conns, macros);
  editor.render();
  return () => editor.stop();
}

class MacroEditor {
  constructor(outlet, macroID, data, meta, conns, macros) {
    this.outlet = outlet;
    this.macroID = macroID;
    this.macro = data.macro;
    this.graph = data.graph;
    this.versionInfo = data.version;
    this.versions = data.versions;
    this.nodeDefs = data.nodeDefs;
    this.issues = data.issues;
    this.permission = data.permission;
    this.specs = meta.specs;
    this.shellEnabled = meta.shellEnabled;
    this.connections = conns.items;
    this.macros = macros.items;

    // 주석은 실행에 관여하지 않지만 그래프와 함께 저장된다.
    // 예전 버전에는 없던 필드이므로 여기서 채워 둔다.
    this.graph.notes = this.graph.notes ?? [];
    this.graph.groups = this.graph.groups ?? [];

    // 선택은 종류가 셋이다(노드·메모·그룹). id만으로는 어느 것인지 알 수 없다.
    this.selected = null;
    this.selKind = 'node';
    this.dirty = false;
    // view는 캔버스의 팬/줌 상태다. 그래프와 함께 저장해 되돌렸을 때 보던 자리가 유지된다.
    this.view = { x: this.graph.view?.x ?? 0, y: this.graph.view?.y ?? 0, k: this.graph.view?.k ?? 1 };
    this.linking = null;
    this.dragging = null;

    this.specByType = new Map(this.specs.map((s) => [s.type, s]));
    this.defByID = new Map(this.nodeDefs.map((d) => [d.id, d]));

    this.onKey = (e) => this.handleKey(e);
    this.onBeforeUnload = (e) => {
      // 저장하지 않은 편집을 들고 페이지를 떠나는 것은 흔한 사고다.
      // 매크로는 실행 전에 저장되어야 하므로, 저장하지 않은 상태는 존재하지 않는
      // 매크로와 같다.
      if (!this.dirty) return;
      e.preventDefault();
      e.returnValue = '';
    };
  }

  stop() {
    document.removeEventListener('keydown', this.onKey);
    window.removeEventListener('beforeunload', this.onBeforeUnload);
    setLeaveGuard(null);
  }

  // ---------- 화면 구성 ----------

  render() {
    this.svg = document.createElementNS(SVG_NS, 'svg');
    this.svg.classList.add('macro-canvas');
    this.viewport = document.createElementNS(SVG_NS, 'g');
    this.svg.appendChild(this.viewport);

    this.panel = h('aside.macro-panel');
    this.palette = h('div.macro-palette');
    this.statusBar = h('div.macro-status');
    // 캔버스 오른쪽 아래에 고정되는 도움말. 마우스를 따라다니지 않는 이유는
    // 따라다니는 상자가 정작 가리키는 노드를 가리기 때문이다.
    this.helpBox = h('div.macro-help', { style: { display: 'none' } });

    const root = h('div.macro-editor', {},
      h('header.macro-head', {},
        h('div.macro-head-main', {},
          h('a.macro-back', { href: '/macros' }, icon('x', 14), '목록'),
          h('h1.macro-title', {}, this.macro.name),
          badge(`v${this.versionInfo.version}`, 'neutral'),
          visibilityBadge(this.macro),
          this.versionInfo.version !== this.macro.currentVersion
            ? badge('과거 버전 보기', 'warn')
            : null,
        ),
        h('div.macro-head-side', {}, this.buildToolbar()),
      ),
      this.statusBar,
      h('div.macro-body', {},
        h('div.macro-side', {}, this.palette),
        h('div.macro-canvas-wrap', {}, this.svg, this.helpBox),
        this.panel,
      ),
    );

    mount(this.outlet, root);
    this.bindCanvas();
    this.drawPalette();
    this.draw();
    this.drawPanel();
    this.drawStatus();

    document.addEventListener('keydown', this.onKey);
    window.addEventListener('beforeunload', this.onBeforeUnload);
    // beforeunload는 새로고침·창 닫기만 잡는다. 앱 안에서 메뉴를 눌러 나가는 것은
    // 브라우저 입장에서 이동이 아니므로 라우터에 따로 물어보게 한다.
    setLeaveGuard(() => this.confirmLeave());
  }

  // confirmLeave는 저장하지 않은 편집을 들고 나가려 할 때 묻는다.
  // 저장 → 이동, 그냥 나가기, 취소 세 갈래다.
  async confirmLeave() {
    if (!this.dirty) return true;
    return new Promise((resolve) => {
      let answered = false;
      const done = (value, close) => {
        answered = true;
        close();
        resolve(value);
      };
      openModal({
        title: '저장하지 않은 편집이 있습니다',
        width: 480,
        body: () => h('p.modal-message', {},
          '이 화면을 떠나면 저장하지 않은 변경은 사라집니다. '
          + '매크로는 저장된 버전으로만 실행되므로, 지금 저장하면 새 버전이 됩니다.'),
        footer: (close) => [
          h('button.btn', { type: 'button', onclick: () => done(false, close) }, '취소'),
          h('button.btn.btn-danger', { type: 'button', onclick: () => done(true, close) },
            '저장하지 않고 나가기'),
          h('button.btn.btn-primary', {
            type: 'button',
            onclick: async () => {
              close();
              answered = true;
              const saved = await this.save({ stay: true });
              // 저장에 실패하면 이동을 막는다. 실패한 채 나가면 편집이 사라진다.
              resolve(saved);
            },
          }, '저장하고 나가기'),
        ],
        onClose: () => { if (!answered) resolve(false); },
      });
    });
  }

  // buildToolbar는 권한에 맞는 버튼만 만든다.
  //
  // 서버가 어차피 막지만, 눌러서 403을 보고 알게 되는 것과 처음부터 없는 것은
  // 다른 경험이다. 무엇을 할 수 없는지는 상태 줄이 한 문장으로 설명한다.
  buildToolbar() {
    this.saveBtn = h('button.btn.btn-small', { type: 'button', onclick: () => this.save() },
      icon('save'), '버전 저장');
    this.runBtn = h('button.btn.btn-small.btn-primary', {
      type: 'button', onclick: () => this.openRunDialog(),
    }, icon('play'), '실행');

    return [
      this.macro.canEdit
        ? h('button.btn.btn-small', { type: 'button', onclick: () => this.openParamsDialog() },
            icon('settings'), '파라미터')
        : null,
      h('button.btn.btn-small', { type: 'button', onclick: () => this.openNodeDefDialog() },
        icon('code'), '사용자 노드'),
      h('button.btn.btn-small', { type: 'button', onclick: () => this.openVersionsDialog() },
        icon('history'), `버전 ${this.versions.length}`),
      this.macro.canManage
        ? h('button.btn.btn-small', { type: 'button', onclick: () => this.openTriggersDialog() },
            icon('refresh'), '자동 실행')
        : null,
      h('button.btn.btn-small', { type: 'button', onclick: () => this.openRunsDialog() },
        icon('list'), '실행 이력'),
      this.macro.canManage
        ? h('button.btn.btn-small', {
            type: 'button',
            onclick: () => openShareDialog(
              { kind: 'macro', id: this.macroID, name: this.macro.name, item: this.macro },
              (updated) => {
                // 배지와 버튼이 바뀐 설정을 그대로 반영해야 한다.
                Object.assign(this.macro, updated);
                navigate(`/macros/${this.macroID}`);
              }),
          }, icon('users'), '공유')
        : null,
      this.macro.canEdit ? this.saveBtn : null,
      this.runBtn,
    ].filter(Boolean);
  }

  // drawStatus는 검증 결과와 실행 권한을 한 줄로 보여준다.
  //
  // 실행 버튼을 그냥 비활성화만 하면 사용자는 이유를 모른 채 "고장났다"고 생각한다.
  // 요구사항이 "경고를 띄우고 비활성화"인 것도 같은 이유다.
  drawStatus() {
    const parts = [];
    const fatal = this.issues.filter((i) => i.fatal);
    const warn = this.issues.filter((i) => !i.fatal);

    if (fatal.length) {
      parts.push(h('p.notice.notice-danger', {}, icon('alert'),
        `저장할 수 없는 오류 ${fatal.length}건: ${fatal.map((i) => i.message).join(' / ')}`));
    }
    if (warn.length) {
      parts.push(h('p.notice.notice-warn', {}, icon('alert'),
        warn.map((i) => i.message).join(' / ')));
    }
    if (!this.permission.canRun) {
      parts.push(h('p.notice.notice-danger', {}, icon('lock'),
        h('span', {},
          h('b', {}, '실행 권한이 부족합니다. '),
          this.permission.blockers.map((b) => `${b.node}: ${b.reason}`).join(' / '),
        )));
    } else if (this.permission.usesCustom) {
      parts.push(h('p.notice.notice-info', {}, icon('alert'),
        '사용자 노드는 스크립트 내용에 따라 실행 중 권한 검사에 걸릴 수 있습니다'));
    }
    if (this.dirty) {
      parts.push(h('p.notice.notice-warn', {}, icon('alert'),
        '저장하지 않은 변경이 있습니다. 실행은 저장된 버전으로만 이루어집니다.'));
    }
    // 읽기 전용인 이유를 밝힌다. 저장 버튼이 없는 것만 보여주면 사용자는
    // 화면이 덜 그려진 것으로 오해한다.
    if (!this.macro.canEdit) {
      parts.push(h('p.notice.notice-info', {}, icon('lock'),
        h('span', {},
          h('b', {}, '조회·실행만 가능합니다. '),
          `${this.macro.createdByName || '만든 사람'}에게 수정 권한(공개 설정 변경 또는 협업자 추가)을 요청하세요.`,
        )));
    }

    this.runBtn.disabled = !this.permission.canRun ||
      this.versionInfo.version !== this.macro.currentVersion;
    if (this.versionInfo.version !== this.macro.currentVersion) {
      this.runBtn.title = '과거 버전은 실행할 수 없습니다. 되돌린 뒤 실행하세요';
    } else if (!this.permission.canRun) {
      this.runBtn.title = '실행 권한이 부족합니다';
    } else {
      this.runBtn.title = '';
    }

    mount(this.statusBar, parts);
  }

  // ---------- 팔레트 ----------

  drawPalette() {
    // 수정 권한이 없으면 팔레트 대신 이유를 둔다.
    //
    // 버튼을 남겨 두고 눌러도 아무 일이 없게 하는 것보다 낫다 — 노드는 놓이는데
    // 저장되지 않는 상태가 가장 나쁘고, 그 다음은 눌러도 반응이 없는 버튼이다.
    // 사용자 노드 목록은 남긴다. 남의 매크로가 무엇을 쓰는지 읽는 것은 조회의 일부다.
    if (!this.macro.canEdit) {
      mount(this.palette,
        h('div.palette-head', {}, '노드'),
        h('p.muted.small', {},
          '조회 전용입니다. 노드를 추가하거나 옮긴 것은 저장되지 않습니다.'),
        h('div.palette-group', {},
          h('div.palette-title', {}, '사용자 노드'),
          this.nodeDefs.length
            ? this.nodeDefs.map((d) => h('button.palette-item', {
                type: 'button',
                onclick: () => this.openNodeDefDialog(d),
              },
                h('span.palette-name', {}, d.name),
                h('span.palette-note', {}, d.scope === 'global' ? '전역' : '이 매크로'),
              ))
            : h('p.muted.small', {}, '등록된 노드가 없습니다'),
        ),
      );
      return;
    }

    const groups = [
      { key: 'flow', label: '흐름' },
      { key: 'db', label: '데이터베이스' },
      { key: 'studio', label: 'DB Studio' },
      { key: 'script', label: '스크립트' },
    ];

    const blocks = groups.map((g) => {
      const items = this.specs.filter((s) => s.group === g.key && s.type !== 'start');
      if (!items.length) return null;
      return h('div.palette-group', {},
        h('div.palette-title', {}, g.label),
        ...items.map((spec) => h('button.palette-item', {
          type: 'button',
          title: spec.description,
          disabled: spec.needsShell && !this.shellEnabled,
          onclick: () => this.addNode(spec.type),
          // 팔레트 항목은 이름만 보인다. 무엇을 하는 노드이고 무엇이 필요한지는
          // 오른쪽 아래 고정 패널에 띄운다 — title 툴팁은 나타나기까지 한참 걸리고
          // 여러 줄을 담지 못한다.
          onmouseenter: () => this.showHelp(spec),
          onmouseleave: () => this.hideHelp(),
        },
          h('span.palette-icon', {}, icon(nodeIconName(spec.type), 13)),
          h('span.palette-name', {}, spec.label),
          spec.needsShell && !this.shellEnabled
            ? h('span.palette-note', {}, '서버 설정 필요')
            : null,
        )),
      );
    });

    const customItems = this.nodeDefs.map((d) => h('button.palette-item', {
      type: 'button', title: d.description,
      onclick: () => this.addNode('custom', d.id),
      onmouseenter: () => this.showHelp({
        label: d.name, description: d.description,
        group: '사용자 노드', ports: safeJSON(d.ports, ['out']),
        fields: safeJSON(d.fields, []),
      }),
      onmouseleave: () => this.hideHelp(),
    },
      h('span.palette-name', {}, d.name),
      h('span.palette-note', {}, d.scope === 'global' ? '전역' : '이 매크로'),
    ));

    mount(this.palette,
      h('div.palette-head', {}, '노드 추가'),
      ...blocks.filter(Boolean),
      h('div.palette-group', {},
        h('div.palette-title', {}, '사용자 노드'),
        customItems.length
          ? customItems
          : h('p.muted.small', {}, '등록된 노드가 없습니다'),
        h('button.link-btn', { type: 'button', onclick: () => this.openNodeDefDialog() },
          '노드 만들기'),
      ),
      // 주석은 실행되지 않는다. 팔레트 맨 아래에 따로 둬서 노드와 섞이지 않게 한다.
      h('div.palette-group', {},
        h('div.palette-title', {}, '주석'),
        h('button.palette-item', {
          type: 'button', title: '캔버스에 메모를 붙입니다 (실행에 영향 없음)',
          onclick: () => this.addNote(),
        }, h('span.palette-name', {}, icon('edit', 13), ' 메모')),
        h('button.palette-item', {
          type: 'button', title: '노드 묶음을 감싸는 사각형을 놓습니다 (실행에 영향 없음)',
          onclick: () => this.addGroup(),
        }, h('span.palette-name', {}, icon('list', 13), ' 그룹')),
      ),
    );
  }

  // ---------- 도움말 ----------

  // showHelp는 노드 명세를 오른쪽 아래 패널에 펼친다.
  // 무엇을 하는지(설명), 무엇이 필요한지(필수 입력·권한), 어디로 이어지는지(포트).
  showHelp(spec) {
    if (!spec) return;
    const fields = spec.fields ?? [];
    const required = fields.filter((f) => f.required);
    const optional = fields.filter((f) => !f.required);
    const needs = [];
    if (spec.needsCap) needs.push(`데이터 능력 ${capLabelOf(spec.needsCap)}`);
    if (spec.needsLevel) needs.push(`등급 ${levelLabelOf(spec.needsLevel)} 이상`);
    if (spec.needsPerm) needs.push(`전역 권한 ${permLabelOf(spec.needsPerm)}`);
    if (spec.needsShell) needs.push('서버 -allow-shell');

    mount(this.helpBox,
      h('div.help-head', {},
        h('span.help-icon', {}, icon(nodeIconName(spec.type), 14)),
        h('strong', {}, spec.label ?? spec.type),
        spec.group ? h('span.help-group', {}, groupLabel(spec.group)) : null,
      ),
      spec.description ? h('p.help-desc', {}, spec.description) : null,
      required.length
        ? h('div.help-row', {}, h('span.help-key', {}, '필수 입력'),
            h('span', {}, required.map((f) => f.label).join(', ')))
        : null,
      optional.length
        ? h('div.help-row', {}, h('span.help-key', {}, '선택 입력'),
            h('span', {}, optional.map((f) => f.label).join(', ')))
        : null,
      spec.ports?.length
        ? h('div.help-row', {}, h('span.help-key', {}, '다음 연결'),
            h('span', {}, spec.ports.join(' · ')))
        : null,
      needs.length
        ? h('div.help-row.is-warn', {}, h('span.help-key', {}, '필요 권한'),
            h('span', {}, needs.join(' · ')))
        : null,
    );
    this.helpBox.style.display = '';
  }

  hideHelp() {
    this.helpBox.style.display = 'none';
  }

  // ---------- 주석(메모·그룹) ----------

  addNote() {
    const center = this.centerPoint();
    const note = {
      id: `m${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`,
      text: '메모',
      x: Math.round(center.x - 90), y: Math.round(center.y - 40),
      w: 180, h: 80, color: 'yellow',
    };
    this.graph.notes.push(note);
    this.select('note', note.id);
    this.markDirty();
    this.draw();
  }

  addGroup() {
    const center = this.centerPoint();
    const group = {
      id: `g${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`,
      label: '그룹',
      x: Math.round(center.x - 160), y: Math.round(center.y - 110),
      w: 320, h: 220, color: 'blue',
    };
    this.graph.groups.push(group);
    this.select('group', group.id);
    this.markDirty();
    this.draw();
  }

  centerPoint() {
    const rect = this.svg.getBoundingClientRect();
    return this.toGraph(rect.width / 2, rect.height / 2);
  }

  select(kind, id) {
    this.selKind = kind;
    this.selected = id;
    this.drawPanel();
    this.highlightSelection();
  }

  selectedItem() {
    if (!this.selected) return null;
    if (this.selKind === 'note') return this.graph.notes.find((n) => n.id === this.selected);
    if (this.selKind === 'group') return this.graph.groups.find((g) => g.id === this.selected);
    return this.graph.nodes.find((n) => n.id === this.selected);
  }

  addNode(type, nodeRef) {
    const id = `n${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
    // 새 노드는 화면 중앙에 놓는다. 원점(0,0)에 놓으면 캔버스를 옮겨 놓은 상태에서
    // 노드가 보이지 않는 곳에 생겨 "추가가 안 된다"로 보인다.
    const rect = this.svg.getBoundingClientRect();
    const center = this.toGraph(rect.width / 2, rect.height / 2);
    const node = {
      id, type, label: '', x: Math.round(center.x - NODE_W / 2), y: Math.round(center.y - 40),
      params: {},
    };
    if (nodeRef) node.nodeRef = nodeRef;
    this.graph.nodes.push(node);
    this.selected = id;
    this.markDirty();
    this.draw();
    this.drawPanel();
  }

  // ---------- 캔버스 ----------

  bindCanvas() {
    this.svg.addEventListener('mousedown', (e) => {
      if (e.target === this.svg || e.target.classList.contains('macro-grid')) {
        this.selected = null;
        this.drawPanel();
        this.startPan(e);
      }
    });
    this.svg.addEventListener('wheel', (e) => {
      e.preventDefault();
      const rect = this.svg.getBoundingClientRect();
      const before = this.toGraph(e.clientX - rect.left, e.clientY - rect.top);
      const factor = e.deltaY < 0 ? 1.1 : 1 / 1.1;
      this.view.k = Math.min(2.5, Math.max(0.25, this.view.k * factor));
      const after = this.toGraph(e.clientX - rect.left, e.clientY - rect.top);
      // 커서 아래의 점이 제자리에 있도록 팬을 보정한다. 그러지 않으면
      // 확대할 때마다 그림이 왼쪽 위로 달아난다.
      this.view.x += (after.x - before.x) * this.view.k;
      this.view.y += (after.y - before.y) * this.view.k;
      this.applyView();
    }, { passive: false });

    // 링크 연결 중 마우스를 놓으면 취소한다. 노드 위에서 놓는 경우는 노드가 처리한다.
    this.svg.addEventListener('mouseup', () => this.cancelLink());
    this.svg.addEventListener('mousemove', (e) => {
      if (!this.linking) return;
      const rect = this.svg.getBoundingClientRect();
      const p = this.toGraph(e.clientX - rect.left, e.clientY - rect.top);
      this.updateLinkPreview(p);
    });
  }

  toGraph(px, py) {
    return { x: (px - this.view.x) / this.view.k, y: (py - this.view.y) / this.view.k };
  }

  applyView() {
    this.viewport.setAttribute('transform',
      `translate(${this.view.x} ${this.view.y}) scale(${this.view.k})`);
  }

  startPan(e) {
    const startX = e.clientX;
    const startY = e.clientY;
    const originX = this.view.x;
    const originY = this.view.y;
    const move = (ev) => {
      this.view.x = originX + (ev.clientX - startX);
      this.view.y = originY + (ev.clientY - startY);
      this.applyView();
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  }

  draw() {
    mount(this.viewport);
    this.applyView();

    // 층을 나눈다: 그룹(배경) → 간선 → 노드 → 메모(맨 위).
    // 메모를 맨 위에 두는 이유는 글자가 무엇에도 가리지 않아야 하기 때문이다.
    this.groupLayer = document.createElementNS(SVG_NS, 'g');
    this.edgeLayer = document.createElementNS(SVG_NS, 'g');
    this.nodeLayer = document.createElementNS(SVG_NS, 'g');
    this.noteLayer = document.createElementNS(SVG_NS, 'g');
    this.viewport.appendChild(this.groupLayer);
    this.viewport.appendChild(this.edgeLayer);
    this.viewport.appendChild(this.nodeLayer);
    this.viewport.appendChild(this.noteLayer);

    for (const group of this.graph.groups) this.drawGroup(group);
    for (const edge of this.graph.edges) this.drawEdge(edge);
    for (const node of this.graph.nodes) this.drawNode(node);
    for (const note of this.graph.notes) this.drawNote(note);
  }

  drawGroup(group) {
    const g = document.createElementNS(SVG_NS, 'g');
    g.classList.add('macro-group', `tint-${group.color || 'blue'}`);
    if (this.selKind === 'group' && group.id === this.selected) g.classList.add('is-selected');
    g.setAttribute('transform', `translate(${group.x} ${group.y})`);
    g.dataset.id = group.id;

    const box = document.createElementNS(SVG_NS, 'rect');
    box.setAttribute('width', group.w);
    box.setAttribute('height', group.h);
    box.setAttribute('rx', 12);
    box.classList.add('group-box');
    g.appendChild(box);

    if (group.label) g.appendChild(svgText(12, 20, group.label, 'group-label'));

    // 크기 손잡이. 그룹은 노드를 담는 것이 아니라 감싸는 것이므로 크기를 사람이 정한다.
    const grip = document.createElementNS(SVG_NS, 'rect');
    grip.setAttribute('x', group.w - 14);
    grip.setAttribute('y', group.h - 14);
    grip.setAttribute('width', 12);
    grip.setAttribute('height', 12);
    grip.setAttribute('rx', 3);
    grip.classList.add('group-grip');
    grip.addEventListener('mousedown', (e) => {
      e.stopPropagation();
      this.select('group', group.id);
      this.startResize(group, e);
    });
    g.appendChild(grip);

    g.addEventListener('mousedown', (e) => {
      if (e.target === grip) return;
      e.stopPropagation();
      this.select('group', group.id);
      this.startDrag(group, e, this.groupLayer);
    });
    this.groupLayer.appendChild(g);
  }

  drawNote(note) {
    const g = document.createElementNS(SVG_NS, 'g');
    g.classList.add('macro-note', `tint-${note.color || 'yellow'}`);
    if (this.selKind === 'note' && note.id === this.selected) g.classList.add('is-selected');
    g.setAttribute('transform', `translate(${note.x} ${note.y})`);
    g.dataset.id = note.id;

    const w = note.w || 180;
    const box = document.createElementNS(SVG_NS, 'rect');
    box.setAttribute('width', w);
    box.setAttribute('height', note.h || 80);
    box.setAttribute('rx', 6);
    box.classList.add('note-box');
    g.appendChild(box);

    // 글줄 나누기는 손으로 한다. SVG text는 자동 줄바꿈이 없다.
    const text = document.createElementNS(SVG_NS, 'text');
    text.classList.add('note-text');
    text.setAttribute('x', 10);
    text.setAttribute('y', 20);
    for (const [i, line] of wrapText(note.text ?? '', Math.floor((w - 20) / 7)).entries()) {
      const tspan = document.createElementNS(SVG_NS, 'tspan');
      tspan.setAttribute('x', 10);
      if (i > 0) tspan.setAttribute('dy', '1.35em');
      tspan.textContent = line;
      text.appendChild(tspan);
    }
    g.appendChild(text);

    const grip = document.createElementNS(SVG_NS, 'rect');
    grip.setAttribute('x', w - 14);
    grip.setAttribute('y', (note.h || 80) - 14);
    grip.setAttribute('width', 12);
    grip.setAttribute('height', 12);
    grip.setAttribute('rx', 3);
    grip.classList.add('group-grip');
    grip.addEventListener('mousedown', (e) => {
      e.stopPropagation();
      this.select('note', note.id);
      this.startResize(note, e);
    });
    g.appendChild(grip);

    g.addEventListener('mousedown', (e) => {
      if (e.target === grip) return;
      e.stopPropagation();
      this.select('note', note.id);
      this.startDrag(note, e, this.noteLayer);
    });
    this.noteLayer.appendChild(g);
  }

  // startResize는 그룹·메모의 크기를 바꾼다.
  startResize(item, e) {
    const rect = this.svg.getBoundingClientRect();
    const start = this.toGraph(e.clientX - rect.left, e.clientY - rect.top);
    const w0 = item.w || 180;
    const h0 = item.h || 80;
    let moved = false;

    const move = (ev) => {
      const p = this.toGraph(ev.clientX - rect.left, ev.clientY - rect.top);
      item.w = Math.max(80, Math.round(w0 + (p.x - start.x)));
      item.h = Math.max(48, Math.round(h0 + (p.y - start.y)));
      moved = true;
      this.draw();
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      if (moved) this.markDirty();
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  }

  nodePorts(node) {
    if (node.type === 'custom') {
      const def = this.defByID.get(node.nodeRef);
      const ports = def ? safeJSON(def.ports, []) : [];
      return ports.length ? ports : ['out'];
    }
    const spec = this.specByType.get(node.type);
    return spec?.ports ?? ['out'];
  }

  nodeHeight(node) {
    const ports = this.nodePorts(node);
    return HEAD_H + BODY_PAD * 2 + Math.max(1, ports.length) * PORT_ROW;
  }

  portPos(node, port) {
    const ports = this.nodePorts(node);
    const index = Math.max(0, ports.indexOf(port));
    return {
      x: node.x + NODE_W,
      y: node.y + HEAD_H + BODY_PAD + index * PORT_ROW + PORT_ROW / 2,
    };
  }

  inputPos(node) {
    return { x: node.x, y: node.y + HEAD_H / 2 };
  }

  drawNode(node) {
    const spec = this.specByType.get(node.type);
    const def = node.type === 'custom' ? this.defByID.get(node.nodeRef) : null;
    const ports = this.nodePorts(node);
    const height = this.nodeHeight(node);

    const g = document.createElementNS(SVG_NS, 'g');
    g.classList.add('macro-node', `node-${(spec?.group ?? 'custom')}`);
    if (node.id === this.selected) g.classList.add('is-selected');
    if (node.disabled) g.classList.add('is-disabled');
    g.setAttribute('transform', `translate(${node.x} ${node.y})`);
    g.dataset.id = node.id;

    const box = document.createElementNS(SVG_NS, 'rect');
    box.setAttribute('width', NODE_W);
    box.setAttribute('height', height);
    box.setAttribute('rx', 8);
    box.classList.add('node-box');
    g.appendChild(box);

    const head = document.createElementNS(SVG_NS, 'rect');
    head.setAttribute('width', NODE_W);
    head.setAttribute('height', HEAD_H);
    head.setAttribute('rx', 8);
    head.classList.add('node-head');
    g.appendChild(head);

    // 아이콘을 이름 왼쪽에 둔다. 노드가 스무 개 넘어가면 글자를 읽기 전에
    // 종류가 눈에 들어와야 원하는 노드를 찾을 수 있다.
    const mark = icon(nodeIconName(node.type), 13);
    mark.setAttribute('x', 9);
    mark.setAttribute('y', 8);
    mark.classList.add('node-icon');
    g.appendChild(mark);

    g.appendChild(svgText(28, 20, node.label || def?.name || spec?.label || node.type, 'node-title'));

    // 입력 포트(시작 노드는 받는 곳이 없다).
    if (node.type !== 'start') {
      const inPos = this.inputPos(node);
      const inDot = document.createElementNS(SVG_NS, 'circle');
      inDot.setAttribute('cx', 0);
      inDot.setAttribute('cy', inPos.y - node.y);
      inDot.setAttribute('r', 5);
      inDot.classList.add('node-port', 'port-in');
      g.appendChild(inDot);
    }

    ports.forEach((port, i) => {
      const y = HEAD_H + BODY_PAD + i * PORT_ROW + PORT_ROW / 2;
      g.appendChild(svgText(NODE_W - 16, y + 4, port, 'node-port-label', 'end'));
      const dot = document.createElementNS(SVG_NS, 'circle');
      dot.setAttribute('cx', NODE_W);
      dot.setAttribute('cy', y);
      dot.setAttribute('r', 5);
      dot.classList.add('node-port', 'port-out');
      dot.dataset.port = port;
      dot.addEventListener('mousedown', (e) => {
        e.stopPropagation();
        this.startLink(node, port);
      });
      g.appendChild(dot);
    });

    // 요약 한 줄: 이 노드가 무엇을 하는지 상자 안에서 알 수 있어야
    // 노드를 하나씩 눌러 보지 않는다.
    const summary = this.nodeSummary(node);
    if (summary) {
      g.appendChild(svgText(10, HEAD_H + BODY_PAD + 14, summary, 'node-summary'));
    }

    g.addEventListener('mousedown', (e) => {
      if (e.target.classList.contains('node-port')) return;
      e.stopPropagation();
      this.selKind = 'node';
      this.selected = node.id;
      this.drawPanel();
      this.highlightSelection();
      this.startDrag(node, e);
    });
    g.addEventListener('mouseup', (e) => {
      if (!this.linking) return;
      e.stopPropagation();
      this.finishLink(node);
    });
    // 캔버스 위의 노드에도 같은 도움말을 붙인다. 남이 만든 매크로를 열었을 때
    // 이 노드가 무엇을 요구하는지 알려면 지금은 눌러서 패널을 봐야 한다.
    g.addEventListener('mouseenter', () => this.showHelp(def
      ? {
        label: def.name, description: def.description, group: '사용자 노드',
        ports, fields: safeJSON(def.fields, []),
      }
      : spec));
    g.addEventListener('mouseleave', () => this.hideHelp());

    this.nodeLayer.appendChild(g);
  }

  nodeSummary(node) {
    const parts = [];
    const connID = node.params?.connection;
    if (connID) {
      const conn = this.connections.find((i) => i.connection.id === connID);
      parts.push(conn ? conn.connection.name : '알 수 없는 커넥션');
    }
    for (const key of ['sql', 'table', 'message', 'expr', 'list', 'name', 'script', 'shell']) {
      const value = node.params?.[key];
      if (typeof value === 'string' && value.trim()) {
        parts.push(oneLine(value, 26));
        break;
      }
    }
    return parts.join(' · ');
  }

  highlightSelection() {
    for (const el of this.nodeLayer.querySelectorAll('.macro-node')) {
      el.classList.toggle('is-selected', el.dataset.id === this.selected);
    }
  }

  drawEdge(edge) {
    const from = this.graph.nodes.find((n) => n.id === edge.from);
    const to = this.graph.nodes.find((n) => n.id === edge.to);
    if (!from || !to) return;
    const a = this.portPos(from, edge.fromPort);
    const b = this.inputPos(to);

    const path = document.createElementNS(SVG_NS, 'path');
    path.setAttribute('d', bezier(a, b));
    path.classList.add('macro-edge');
    path.addEventListener('click', async (e) => {
      e.stopPropagation();
      const ok = await confirmDialog({
        title: '연결 삭제', message: '이 연결을 지웁니다.', confirmLabel: '삭제', danger: true,
      });
      if (!ok) return;
      this.graph.edges = this.graph.edges.filter((x) => x.id !== edge.id);
      this.markDirty();
      this.draw();
    });
    this.edgeLayer.appendChild(path);
  }

  // layer를 받는 이유: 노드·메모·그룹이 서로 다른 층에 있고 끄는 방식은 같다.
  startDrag(node, e, layer = null) {
    const rect = this.svg.getBoundingClientRect();
    const start = this.toGraph(e.clientX - rect.left, e.clientY - rect.top);
    const originX = node.x;
    const originY = node.y;
    const host = layer ?? this.nodeLayer;
    let moved = false;

    const move = (ev) => {
      const p = this.toGraph(ev.clientX - rect.left, ev.clientY - rect.top);
      node.x = Math.round(originX + (p.x - start.x));
      node.y = Math.round(originY + (p.y - start.y));
      moved = true;
      // 드래그 중에는 전체 재생성 대신 좌표만 옮긴다. 노드가 수십 개인 그래프에서도
      // 마우스를 따라오는 느낌이 유지되어야 한다.
      const el = host.querySelector(`[data-id="${node.id}"]`);
      if (el) el.setAttribute('transform', `translate(${node.x} ${node.y})`);
      if (host !== this.nodeLayer) return; // 주석에는 간선이 붙지 않는다
      mount(this.edgeLayer);
      for (const edge of this.graph.edges) this.drawEdge(edge);
    };
    const up = () => {
      document.removeEventListener('mousemove', move);
      document.removeEventListener('mouseup', up);
      if (moved) this.markDirty();
    };
    document.addEventListener('mousemove', move);
    document.addEventListener('mouseup', up);
  }

  startLink(node, port) {
    this.linking = { from: node, port };
    this.preview = document.createElementNS(SVG_NS, 'path');
    this.preview.classList.add('macro-edge', 'is-preview');
    this.edgeLayer.appendChild(this.preview);
  }

  updateLinkPreview(p) {
    if (!this.preview) return;
    const a = this.portPos(this.linking.from, this.linking.port);
    this.preview.setAttribute('d', bezier(a, p));
  }

  cancelLink() {
    if (!this.linking) return;
    this.preview?.remove();
    this.preview = null;
    this.linking = null;
  }

  finishLink(target) {
    const { from, port } = this.linking;
    this.cancelLink();
    if (target.id === from.id) {
      toast('자기 자신에게 연결할 수 없습니다', 'error');
      return;
    }
    if (target.type === 'start') {
      toast('시작 노드로는 연결할 수 없습니다', 'error');
      return;
    }
    const exists = this.graph.edges.some((e) =>
      e.from === from.id && e.fromPort === port && e.to === target.id);
    if (exists) return;

    this.graph.edges.push({
      id: `e${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`,
      from: from.id, fromPort: port, to: target.id,
    });
    this.markDirty();
    this.draw();
  }

  handleKey(e) {
    if (e.target.matches('input, textarea, select')) return;
    if ((e.key === 'Delete' || e.key === 'Backspace') && this.selected) {
      e.preventDefault();
      this.deleteSelected();
    }
    if ((e.ctrlKey || e.metaKey) && e.key === 's') {
      e.preventDefault();
      this.save();
    }
  }

  deleteSelected() {
    if (this.selKind === 'note') {
      this.graph.notes = this.graph.notes.filter((n) => n.id !== this.selected);
    } else if (this.selKind === 'group') {
      this.graph.groups = this.graph.groups.filter((g) => g.id !== this.selected);
    } else {
      const node = this.graph.nodes.find((n) => n.id === this.selected);
      if (!node) return;
      if (node.type === 'start') {
        toast('시작 노드는 지울 수 없습니다', 'error');
        return;
      }
      this.graph.nodes = this.graph.nodes.filter((n) => n.id !== node.id);
      this.graph.edges = this.graph.edges.filter((e) => e.from !== node.id && e.to !== node.id);
    }
    this.selected = null;
    this.markDirty();
    this.draw();
    this.drawPanel();
  }

  // markDirty는 편집이 있었음을 기록한다. 모든 변경이 여기를 지난다.
  //
  // 수정 권한이 없으면 편집 표시를 남기지 않는다. 남기면 "저장하지 않은 변경이
  // 있습니다"와 나갈 때의 확인 창이 뜨는데, 정작 저장할 방법이 없다. 그 상태에서
  // 사용자가 할 수 있는 것은 자기가 무엇을 잘못했는지 찾는 것뿐이다.
  // 캔버스를 움직여 보는 것 자체는 막지 않는다 — 읽는 데 필요한 동작이다.
  markDirty() {
    if (!this.macro.canEdit) return;
    this.dirty = true;
    this.graph.view = { x: this.view.x, y: this.view.y, k: this.view.k };
    this.drawStatus();
  }

  // ---------- 인스펙터 ----------

  // drawAnnotationPanel은 메모·그룹의 설정을 보여준다.
  drawAnnotationPanel(item) {
    const isNote = this.selKind === 'note';
    const textCtl = isNote
      ? textarea({ value: item.text ?? '', rows: 5 })
      : input({ value: item.label ?? '', placeholder: '예: 백업 → 검증' });
    textCtl.addEventListener('input', () => {
      if (isNote) item.text = textCtl.value;
      else item.label = textCtl.value;
      this.markDirty();
      this.draw();
    });

    const colorRow = h('div.tint-picker', {}, TINTS.map((t) => {
      const btn = h('button.tint-swatch', {
        type: 'button',
        class: `tint-${t.key}${(item.color || (isNote ? 'yellow' : 'blue')) === t.key ? ' is-on' : ''}`,
        title: t.label,
        onclick: () => {
          item.color = t.key;
          this.markDirty();
          this.draw();
          this.drawPanel();
        },
      });
      return btn;
    }));

    mount(this.panel,
      h('div.panel-head', {},
        h('h2', {}, isNote ? '메모' : '그룹'),
        h('button.icon-btn.danger', {
          type: 'button', title: '지우기', onclick: () => this.deleteSelected(),
        }, icon('trash')),
      ),
      h('div.panel-body', {},
        h('label.field', {},
          h('span.field-label', {}, isNote ? '내용' : '이름'),
          textCtl,
        ),
        h('div.field', {}, h('span.field-label', {}, '색'), colorRow),
        h('p.field-help', {},
          '주석은 실행에 영향을 주지 않습니다. 버전과 함께 저장되므로 되돌리면 함께 돌아옵니다.'),
      ),
    );
  }

  drawPanel() {
    if (this.selKind === 'note' || this.selKind === 'group') {
      const item = this.selectedItem();
      if (item) {
        this.drawAnnotationPanel(item);
        return;
      }
    }
    const node = this.graph.nodes.find((n) => n.id === this.selected);
    if (!node) {
      mount(this.panel,
        h('div.panel-empty', {},
          icon('workflow', 28),
          h('p', {}, '노드를 선택하면 설정이 여기에 나옵니다'),
          h('p.muted.small', {},
            '왼쪽 팔레트에서 노드를 추가하고, 오른쪽 점을 끌어 다음 노드에 연결하세요.'),
        ));
      return;
    }

    const spec = this.specByType.get(node.type);
    const def = node.type === 'custom' ? this.defByID.get(node.nodeRef) : null;
    const fields = def ? safeJSON(def.fields, []) : (spec?.fields ?? []);

    const labelInput = input({ value: node.label ?? '', placeholder: spec?.label ?? def?.name ?? '' });
    labelInput.addEventListener('input', () => {
      node.label = labelInput.value;
      this.markDirty();
      this.draw();
    });

    const outputInput = input({ value: node.output ?? '', placeholder: node.id });
    outputInput.addEventListener('input', () => {
      node.output = outputInput.value.trim();
      this.markDirty();
    });

    const controls = fields.map((f) => this.fieldControl(node, f));

    mount(this.panel,
      h('div.panel-head', {},
        h('h2', {}, def?.name ?? spec?.label ?? node.type),
        h('button.icon-btn.danger', {
          type: 'button', title: '노드 삭제', onclick: () => this.deleteSelected(),
        }, icon('trash')),
      ),
      h('p.panel-desc', {}, def?.description || spec?.description || ''),
      field('표시 이름', labelInput, '비워두면 노드 종류 이름을 씁니다'),
      ...controls,
      h('details.panel-advanced', {},
        h('summary', {}, '고급'),
        field('결과 변수 이름', outputInput,
          '이 노드의 결과를 담을 변수입니다. 다른 노드에서 ${이름} 으로 참조합니다'),
        h('label.checkbox', {},
          h('input', {
            type: 'checkbox', checked: Boolean(node.disabled),
            onchange: (e) => {
              node.disabled = e.target.checked;
              this.markDirty();
              this.draw();
            },
          }),
          h('span', {}, '이 노드를 건너뛰기')),
        h('label.checkbox', {},
          h('input', {
            type: 'checkbox', checked: Boolean(node.continueOnError),
            onchange: (e) => {
              node.continueOnError = e.target.checked;
              this.markDirty();
            },
          }),
          h('span', {}, '실패해도 계속 진행')),
      ),
      h('p.panel-hint', {},
        '설정값에 ', h('code', {}, '${변수}'), ' 를 쓰면 실행 시점의 값으로 바뀝니다.'),
    );
  }

  fieldControl(node, f) {
    const value = node.params?.[f.key];
    // redraw를 끌 수 있게 둔 이유: 노드 상자에는 설정값 요약이 한 줄 나오므로
    // 값이 바뀌면 캔버스를 다시 그려야 한다. 하지만 코드 칸은 글자마다 바뀌고,
    // 그때마다 그래프 전체를 다시 그리면 타자가 밀린다. 코드 칸은 포커스를 잃을 때 그린다.
    const setValue = (v, redraw = true) => {
      node.params = node.params ?? {};
      node.params[f.key] = v;
      this.markDirty();
      if (redraw) this.draw();
    };

    let control;
    switch (f.type) {
      case 'connection': {
        // 커넥션 목록은 "이 사람이 접근 가능한 것"만 보여준다. 접근할 수 없는
        // 커넥션을 고를 수 있으면 실행 직전에야 막힌다.
        //
        // "전체 DB"는 그 값을 처리할 줄 아는 노드(백업)에만 나온다. 서버가
        // allowAll로 알려주므로 화면이 노드 종류를 따로 알 필요가 없다.
        const options = [{ value: '', label: '선택하세요' },
          ...(f.allowAll ? [{ value: '*', label: '전체 DB (접근 가능한 모든 DB)' }] : []),
          ...this.connections
            .filter((i) => i.accessible)
            .map((i) => ({ value: i.connection.id, label: i.connection.name }))];
        control = select(options, { value: value ?? '' });
        control.addEventListener('change', () => setValue(control.value));
        break;
      }
      case 'macro': {
        const options = [{ value: '', label: '선택하세요' },
          ...this.macros
            .filter((m) => m.id !== this.macroID)
            .map((m) => ({ value: m.id, label: m.name }))];
        control = select(options, { value: value ?? '' });
        control.addEventListener('change', () => setValue(control.value));
        break;
      }
      case 'select': {
        const options = [{ value: '', label: '(기본)' },
          ...(f.options ?? []).map((o) => ({ value: o, label: o }))];
        control = select(options, { value: value ?? '' });
        control.addEventListener('change', () => setValue(control.value));
        break;
      }
      case 'boolean':
        control = checkbox(f.label, {
          checked: Boolean(value),
          onchange: (e) => setValue(e.target.checked),
        });
        return h('div.field', {}, control,
          f.help ? h('span.field-help', {}, f.help) : null);
      case 'code': {
        // 강조 언어는 필드 정의(FieldSpec.Language)가 정한다. SQL 칸에 Lua 규칙을
        // 씌우면 낱말이 엉뚱하게 칠해져 오히려 읽기 어려워진다.
        //
        // setValue는 draw()를 부르는데, 그러면 캔버스만 다시 그려지고 패널은 그대로다.
        // 편집 중인 입력이 살아 있어야 하므로 여기서 패널을 다시 그리지 않는다.
        const editor = codeEditor({
          value: value ?? '', language: f.language ?? 'sql', rows: 8,
          placeholder: f.placeholder ?? '',
          onInput: (v) => setValue(v, false),
        });
        editor.textarea.addEventListener('blur', () => this.draw());
        return field(f.label + (f.required ? ' *' : ''), editor.el, f.help);
      }
      case 'textarea':
        control = textarea({ value: value ?? '', rows: 3, placeholder: f.placeholder ?? '' });
        control.addEventListener('input', () => setValue(control.value));
        break;
      case 'number':
        control = input({ type: 'number', value: value ?? '', placeholder: f.placeholder ?? '' });
        control.addEventListener('input', () => setValue(control.value));
        break;
      default:
        control = input({ value: value ?? '', placeholder: f.placeholder ?? '', autocomplete: 'off' });
        control.addEventListener('input', () => setValue(control.value));
    }
    return field(f.label + (f.required ? ' *' : ''), control, f.help);
  }

  // ---------- 저장 / 실행 ----------

  // stay가 참이면 저장만 하고 화면을 옮기지 않는다(이탈 확인에서 부를 때).
  // 성공 여부를 돌려주므로 호출부가 다음 동작을 정할 수 있다.
  async save({ stay = false } = {}) {
    const note = await promptText('버전 메모', '무엇을 바꿨는지 적어두면 이력에서 찾기 쉽습니다');
    if (note === null) return false;

    this.saveBtn.disabled = true;
    try {
      const res = await api.post(`/macros/${this.macroID}/versions`, {
        graph: JSON.stringify(this.graph),
        note,
      });
      this.dirty = false;
      this.drawStatus();
      toast(`v${res.version} 으로 저장했습니다`, 'success');
      if (!stay) navigate(`/macros/${this.macroID}`);
      this.saveBtn.disabled = false;
      return true;
    } catch (err) {
      // 검증 오류는 어느 노드가 문제인지까지 알려준다.
      if (err.payload?.issues) {
        this.issues = err.payload.issues;
        this.drawStatus();
      }
      toastError(err);
      this.saveBtn.disabled = false;
      return false;
    }
  }

  openRunDialog() {
    if (this.dirty) {
      toast('저장하지 않은 변경이 있습니다. 저장된 버전이 실행됩니다.', 'warn');
    }
    const params = this.graph.params ?? [];
    const inputs = new Map();

    openModal({
      title: `실행 — ${this.macro.name} v${this.macro.currentVersion}`,
      width: 560,
      body: () => [
        params.length === 0
          ? h('p.muted', {}, '이 매크로는 실행 파라미터가 없습니다.')
          : h('div', {}, params.map((p) => {
              const control = p.type === 'boolean'
                ? h('input', { type: 'checkbox', checked: Boolean(p.default) })
                : p.type === 'connection'
                  ? select([{ value: '', label: '선택하세요' },
                      ...this.connections.filter((i) => i.accessible)
                        .map((i) => ({ value: i.connection.id, label: i.connection.name }))],
                    { value: p.default ?? '' })
                  : input({
                      value: p.default ?? '',
                      type: p.type === 'number' ? 'number' : 'text',
                    });
              inputs.set(p.name, { control, def: p });
              return field(p.label || p.name, control, p.help);
            })),
        this.permission.usesShell
          ? h('p.notice.notice-warn', {}, icon('alert'),
              '이 매크로는 셸 스크립트를 실행합니다.')
          : null,
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: async () => {
            const payload = {};
            for (const [name, { control, def }] of inputs) {
              payload[name] = def.type === 'boolean' ? control.checked : control.value;
            }
            try {
              const res = await api.post(`/macros/${this.macroID}/run`, { params: payload });
              close();
              openRunLog(res.runId);
            } catch (err) {
              toastError(err);
            }
          },
        }, icon('play'), '실행'),
      ],
    });
  }

  // ---------- 파라미터 정의 ----------

  openParamsDialog() {
    const list = h('div.param-list');
    const params = (this.graph.params ?? []).map((p) => ({ ...p }));

    const draw = () => {
      mount(list, params.length === 0
        ? h('p.muted.small', {}, '정의된 파라미터가 없습니다')
        : params.map((p, i) => h('div.param-row', {},
            input({
              value: p.name, placeholder: '이름',
              oninput: (e) => { p.name = e.target.value.trim(); },
            }),
            input({
              value: p.label ?? '', placeholder: '표시 이름',
              oninput: (e) => { p.label = e.target.value; },
            }),
            select([
              { value: 'string', label: '문자열' },
              { value: 'text', label: '여러 줄' },
              { value: 'number', label: '숫자' },
              { value: 'boolean', label: '참/거짓' },
              { value: 'connection', label: '커넥션' },
            ], { value: p.type ?? 'string', onchange: (e) => { p.type = e.target.value; } }),
            input({
              value: p.default ?? '', placeholder: '기본값',
              oninput: (e) => { p.default = e.target.value; },
            }),
            h('label.checkbox.small', {},
              h('input', {
                type: 'checkbox', checked: Boolean(p.required),
                onchange: (e) => { p.required = e.target.checked; },
              }),
              h('span', {}, '필수')),
            h('button.icon-btn.danger', {
              type: 'button', onclick: () => { params.splice(i, 1); draw(); },
            }, icon('trash')),
          )));
    };
    draw();

    openModal({
      title: '실행 파라미터',
      width: 760,
      body: () => [
        h('p.field-help', {},
          '실행할 때 입력받는 값입니다. 노드 설정에서 ${이름} 으로 참조합니다.'),
        list,
        h('button.btn.btn-small', {
          type: 'button',
          onclick: () => { params.push({ name: '', type: 'string' }); draw(); },
        }, icon('plus'), '파라미터 추가'),
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => {
            this.graph.params = params.filter((p) => p.name);
            this.markDirty();
            close();
            toast('파라미터를 반영했습니다. 저장해야 유지됩니다.', 'info');
          },
        }, '적용'),
      ],
    });
  }

  // ---------- 버전 ----------

  openVersionsDialog() {
    openModal({
      title: '버전 이력',
      width: 720,
      body: (close) => h('div.version-list', {}, this.versions.map((v) =>
        h('div.version-row', {},
          h('div', {},
            h('div.version-title', {},
              h('b', {}, `v${v.version}`),
              v.version === this.macro.currentVersion ? badge('현재', 'success') : null,
              v.version === this.versionInfo.version && v.version !== this.macro.currentVersion
                ? badge('보는 중', 'info') : null,
            ),
            h('div.version-note', {}, v.note || '(메모 없음)'),
            h('div.muted.small', {}, `${v.authorName} · ${formatDate(v.createdAt)}`),
          ),
          h('div.version-actions', {},
            h('button.btn.btn-small', {
              type: 'button',
              onclick: () => { close(); navigate(`/macros/${this.macroID}?version=${v.version}`); },
            }, '열어보기'),
            v.version === this.macro.currentVersion ? null : h('button.btn.btn-small', {
              type: 'button',
              onclick: async () => {
                const ok = await confirmDialog({
                  title: '이 버전으로 되돌리기',
                  message: `v${v.version} 의 내용으로 새 버전을 만듭니다. 지금까지의 버전은 그대로 남습니다.`,
                  confirmLabel: '되돌리기',
                });
                if (!ok) return;
                try {
                  const res = await api.post(
                    `/macros/${this.macroID}/versions/${v.version}/restore`);
                  close();
                  toast(`v${res.version} 으로 되돌렸습니다`, 'success');
                  navigate(`/macros/${this.macroID}`);
                } catch (err) {
                  toastError(err);
                }
              },
            }, icon('refresh'), '되돌리기'),
          ),
        ))),
    });
  }

  // openTriggersDialog는 이 매크로의 자동 실행 설정을 연다.
  //
  // 캔버스 옆이 아니라 대화상자에 두는 이유: 트리거는 그래프를 고칠 때 함께 보는 것이
  // 아니라 "이걸 언제 돌릴까"를 정할 때 한 번 여는 화면이다. 옆에 항상 띄워 두면
  // 캔버스가 좁아진다.
  openTriggersDialog() {
    openModal({
      title: `자동 실행 — ${this.macro.name}`,
      width: 780,
      body: () => triggerPanel(this.macroID, this.graph.params ?? []),
    });
  }

  async openRunsDialog() {
    let res;
    try {
      res = await api.get(`/macros/runs?macro=${encodeURIComponent(this.macroID)}&limit=50`);
    } catch (err) {
      toastError(err);
      return;
    }
    openModal({
      title: '실행 이력',
      width: 720,
      body: () => res.items.length === 0
        ? h('p.muted', {}, '실행 기록이 없습니다')
        : h('div.run-list', {}, res.items.map((r) => h('button.run-row', {
            type: 'button', onclick: () => openRunLog(r.id),
          },
            runStatusBadge(r.status),
            h('span', {}, `v${r.version}`),
            h('span', {}, r.actorName),
            h('span.muted', {}, relativeTime(r.startedAt)),
            h('span.muted', {}, r.status === 'running' ? '진행 중' : `${(r.durationMs / 1000).toFixed(1)}초`),
          ))),
    });
  }

  // ---------- 사용자 노드 ----------

  // openNodeDefDialog는 사용자 노드를 만들거나 고친다.
  //
  // 권한은 노드마다 다르다. 전역 노드는 만든 사람의 것이고, 매크로 전용 노드는
  // 소속 매크로의 권한을 그대로 따른다(서버가 그렇게 계산해 access로 내려준다).
  // 그래서 목록의 어떤 노드는 열어도 읽기만 된다.
  openNodeDefDialog(existing) {
    const canEdit = existing ? existing.canEdit : true;
    const name = input({ value: existing?.name ?? '', placeholder: '슬랙 알림', disabled: !canEdit });
    const description = input({ value: existing?.description ?? '', disabled: !canEdit });
    const scope = select([
      { value: 'global', label: '전역 (모든 매크로에서 사용)' },
      // 매크로 전용 노드는 그 매크로를 고칠 수 있어야 만들 수 있다.
      ...(this.macro.canEdit ? [{ value: 'macro', label: '이 매크로 전용' }] : []),
    ], { value: existing?.scope ?? 'global', disabled: !!existing || !canEdit });
    const ports = input({
      value: (safeJSON(existing?.ports, []) ?? []).join(', '),
      placeholder: 'out',
    });
    const fields = codeEditor({
      value: existing ? prettyJSON(existing.fields) : '[]',
      language: 'json', rows: 5,
    });
    const script = codeEditor({
      value: existing?.script ?? DEFAULT_NODE_SCRIPT,
      language: 'lua', rows: 14,
    });
    const note = input({ placeholder: '무엇을 바꿨는지' });

    // 코드 편집기에는 disabled 개념이 없다(textarea 위에 하이라이트를 겹쳐 그린다).
    // 읽기 전용은 textarea에 직접 건다 — 그래야 선택과 복사는 그대로 되고 편집만 막힌다.
    if (!canEdit) {
      for (const ed of [fields, script]) {
        ed.el.querySelector('textarea')?.setAttribute('readonly', 'readonly');
      }
    }

    const save = async (close) => {
      let parsedFields;
      try {
        parsedFields = JSON.parse(fields.value || '[]');
      } catch (err) {
        toast(`설정 필드 JSON을 읽을 수 없습니다: ${err.message}`, 'error');
        return;
      }
      const body = {
        name: name.value.trim(),
        description: description.value.trim(),
        scope: scope.value,
        macroId: scope.value === 'macro' ? this.macroID : '',
        ports: ports.value.split(',').map((s) => s.trim()).filter(Boolean),
        fields: parsedFields,
        script: script.value,
        note: note.value.trim(),
      };
      try {
        if (existing) await api.put(`/macros/nodes/${existing.id}`, body);
        else await api.post('/macros/nodes', body);
        close();
        toast('노드를 저장했습니다', 'success');
        navigate(`/macros/${this.macroID}`);
      } catch (err) {
        toastError(err);
      }
    };

    openModal({
      title: existing ? `사용자 노드 수정 — ${existing.name}` : '사용자 노드 만들기',
      width: 780,
      body: () => [
        !existing && this.nodeDefs.length
          ? h('div.nodedef-list', {},
              h('div.field-label', {}, '등록된 노드'),
              ...this.nodeDefs.map((d) => h('button.nodedef-row', {
                type: 'button',
                onclick: () => this.openNodeDefDialog(d),
              },
                h('b', {}, d.name),
                badge(d.scope === 'global' ? '전역' : '이 매크로', 'neutral'),
                badge(`v${d.currentVersion}`, 'neutral'),
                // 전역 노드만 자체 공개 설정을 가진다. 매크로 전용 노드의 배지는
                // 매크로의 것을 되풀이할 뿐이라 오히려 헷갈린다.
                d.scope === 'global' ? visibilityBadge(d) : null,
                h('span.muted.small', {}, d.description || ''),
              )),
            )
          : null,
        existing && !canEdit
          ? h('p.notice.notice-info', {}, icon('lock'),
              existing.access === 'view'
                ? '이 노드는 조회만 허용되어 있습니다.'
                : '이 노드를 수정할 권한이 없습니다.')
          : null,
        h('div.form-grid', {},
          field('이름', name),
          field('범위', scope),
        ),
        field('설명', description),
        field('출력 포트', ports, '쉼표로 구분합니다. 비워두면 out 하나입니다'),
        field('설정 필드 (JSON)', fields.el,
          '예: [{"key":"url","label":"주소","type":"text"}] — 값은 스크립트에서 params.url 로 읽습니다'),
        field('Lua 스크립트', script.el,
          'vars 로 변수를, params 로 설정값을 읽습니다. return 값이 노드 결과이고, 두 번째 반환값으로 포트를 고릅니다'),
        existing && canEdit ? field('변경 메모', note) : null,
        existing
          ? h('div.form-actions', {},
              existing.scope === 'global' && existing.canManage
                ? h('button.btn.btn-small', {
                    type: 'button',
                    onclick: () => openShareDialog(
                      { kind: 'node', id: existing.id, name: existing.name, item: existing },
                      () => navigate(`/macros/${this.macroID}`)),
                  }, icon('users'), '공유')
                : null,
              existing.canDelete
                ? h('button.btn.btn-small.btn-danger-ghost', {
                    type: 'button',
                    onclick: async () => {
                      const ok = await confirmDialog({
                        title: '노드 삭제',
                        message: `${existing.name} 을(를) 삭제합니다. 이 노드를 쓰는 매크로는 실행할 수 없게 됩니다.`,
                        confirmLabel: '삭제', danger: true,
                      });
                      if (!ok) return;
                      try {
                        await api.del(`/macros/nodes/${existing.id}`);
                        toast('노드를 삭제했습니다', 'success');
                        navigate(`/macros/${this.macroID}`);
                      } catch (err) {
                        toastError(err);
                      }
                    },
                  }, icon('trash'), '이 노드 삭제')
                : null)
          : null,
      ],
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, canEdit ? '취소' : '닫기'),
        canEdit
          ? h('button.btn.btn-primary', { type: 'button', onclick: () => save(close) }, '저장')
          : null,
      ].filter(Boolean),
    });
  }
}

const DEFAULT_NODE_SCRIPT = `-- vars: 실행 문맥의 변수, params: 이 노드의 설정값
-- log.info / db.query / sh.run 등을 쓸 수 있습니다(실행자 권한으로 검사됩니다).
log.info("사용자 노드 실행")
return { ok = true }`;

// ---------- 작은 도구 ----------

// 주석 색. 브랜드 색이 아니라 "구분되는 몇 가지"면 충분하다.
const TINTS = [
  { key: 'yellow', label: '노랑' },
  { key: 'blue', label: '파랑' },
  { key: 'green', label: '초록' },
  { key: 'pink', label: '분홍' },
  { key: 'gray', label: '회색' },
];

// 노드 종류별 아이콘. 없는 종류는 그룹 기본값으로 떨어진다.
const NODE_ICONS = {
  start: 'play', log: 'list', setvar: 'code', branch: 'workflow', foreach: 'refresh',
  delay: 'history', fail: 'alert',
  'sql.query': 'terminal', 'sql.exec': 'terminal',
  'data.query': 'table', 'data.mutate': 'edit',
  'schema.introspect': 'database', 'schema.capture': 'save',
  'drift.check': 'alert', 'connection.test': 'activity', 'backup.create': 'save',
  'macro.call': 'workflow',
  shell: 'terminal', 'http.request': 'activity', lua: 'code',
  custom: 'code',
};

function nodeIconName(type) {
  return NODE_ICONS[type] ?? 'workflow';
}

function groupLabel(key) {
  return { flow: '흐름', db: '데이터베이스', studio: 'DB Studio', script: '스크립트' }[key] ?? key;
}

function capLabelOf(cap) {
  return { 'data.read': '데이터 조회', 'data.write': '데이터 수정', 'sql.run': 'SQL 실행' }[cap] ?? cap;
}

function levelLabelOf(level) {
  return { monitor: '모니터링', erd: 'ERD 설계', migrate: '마이그레이션' }[level] ?? level;
}

function permLabelOf(perm) {
  return { macro: '매크로 사용', 'script.run': '셸 스크립트 실행', 'http.call': '외부 API 호출' }[perm] ?? perm;
}

// wrapText는 글자 수 기준으로 줄을 나눈다.
// SVG text에는 자동 줄바꿈이 없어 메모가 상자 밖으로 흘러나간다.
function wrapText(text, perLine) {
  const width = Math.max(8, perLine);
  const out = [];
  for (const paragraph of String(text ?? '').split('\n')) {
    if (!paragraph) {
      out.push('');
      continue;
    }
    let line = '';
    for (const word of paragraph.split(/\s+/)) {
      // 한 낱말이 줄보다 길면(URL 등) 그대로 두고 넘긴다. 글자 단위로 자르면
      // 읽을 수 없는 조각이 된다.
      if (!line) line = word;
      else if (line.length + 1 + word.length <= width) line += ` ${word}`;
      else {
        out.push(line);
        line = word;
      }
      if (out.length >= 12) return out; // 상자를 넘길 만큼은 그리지 않는다
    }
    if (line) out.push(line);
  }
  return out;
}

function svgText(x, y, text, className, anchor) {
  const el = document.createElementNS(SVG_NS, 'text');
  el.setAttribute('x', x);
  el.setAttribute('y', y);
  if (anchor) el.setAttribute('text-anchor', anchor);
  el.classList.add(className);
  el.textContent = text;
  return el;
}

// bezier는 출력 포트에서 입력 포트로 가는 곡선이다.
// 직선으로 그리면 노드가 겹칠 때 어디서 어디로 가는지 구분되지 않는다.
function bezier(a, b) {
  const dx = Math.max(40, Math.abs(b.x - a.x) / 2);
  return `M ${a.x} ${a.y} C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`;
}

function oneLine(s, max) {
  const flat = s.replace(/\s+/g, ' ').trim();
  return flat.length > max ? `${flat.slice(0, max)}…` : flat;
}

function safeJSON(raw, fallback) {
  if (!raw) return fallback;
  if (typeof raw !== 'string') return raw;
  try {
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

function prettyJSON(raw) {
  const parsed = safeJSON(raw, []);
  return JSON.stringify(parsed, null, 2);
}

// promptText는 한 줄 입력을 받는 모달을 Promise로 감싼다.
// window.prompt는 CSP·모달 스타일과 어울리지 않고 브라우저마다 다르게 보인다.
function promptText(title, help) {
  return new Promise((resolve) => {
    const box = input({ autocomplete: 'off' });
    let settled = false;
    openModal({
      title,
      width: 480,
      body: () => field('메모', box, help),
      footer: (close) => [
        h('button.btn', { type: 'button', onclick: close }, '취소'),
        h('button.btn.btn-primary', {
          type: 'button',
          onclick: () => { settled = true; close(); resolve(box.value.trim()); },
        }, '저장'),
      ],
      onClose: () => { if (!settled) resolve(null); },
    });
  });
}

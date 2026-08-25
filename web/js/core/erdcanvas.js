// ERD 캔버스: 스키마와 레이아웃을 SVG로 그리고, 이동·확대·선택을 다룬다.
//
// 편집기(협업 op-log)와 구조 화면(읽기 전용 + 개인 배치)이 같은 그림을 보여줘야
// 한다. 두 곳에 같은 코드를 두면 언젠가 갈라지고, 그러면 "설계 화면과 구조 화면의
// 같은 테이블이 다르게 보인다"가 된다 — 그 순간 둘 다 못 믿게 된다.
//
// 그래서 이 파일은 **그리는 일과 손으로 옮기는 일만** 안다. 옮긴 결과를 어디에
// 저장할지(서버 op / 개인 뷰)는 콜백으로 바깥이 정한다.
//
// 렌더링 전략: 구조가 바뀌면 통째로 다시 그린다. 부분 갱신은 변경 종류마다 다른
// 경로가 필요하고, 그 경로가 데이터와 어긋나면 화면마다 다른 그림이 나온다.
// 예외는 드래그 중 좌표뿐이다 — 왕복이나 재렌더를 기다리면 카드가 마우스를 못 따라온다.
import { h, mount, icon } from './dom.js';

const SVG_NS = 'http://www.w3.org/2000/svg';

// 카드 치수. 서버의 자동 배치 격자(320×260)와 어긋나면 초기 화면에서 겹친다.
export const CARD_W = 260;
const HEAD_H = 30;
const ROW_H = 20;
const CARD_PAD = 8;
const MAX_VISIBLE_ROWS = 14; // 이보다 많으면 "…N개 더"로 접는다

export class ErdCanvas {
  // wrap은 캔버스를 담을 요소다. 이 클래스가 그 안에 svg를 만든다.
  constructor(wrap, opts = {}) {
    this.wrap = wrap;
    this.opts = opts;
    this.doc = { schema: { tables: [] }, layout: {}, notes: [], groups: [] };
    this.selection = null;
    this.participants = [];
    this.youClientId = null;
    this.remoteCursors = new Map();
    this.view = { x: 0, y: 0, w: 1200, h: 800 };
    this.drag = null;

    this.svg = document.createElementNS(SVG_NS, 'svg');
    this.svg.classList.add('erd-canvas');
    this.svg.setAttribute('preserveAspectRatio', 'xMidYMid meet');
    mount(wrap, this.svg);

    this.bind();
  }

  // canEdit은 "지금 손으로 옮길 수 있는가"다. 구조 화면에서도 배치는 옮길 수
  // 있으므로 스키마 편집 권한과 다른 축이다.
  get canEdit() {
    return this.opts.canEdit ? this.opts.canEdit() !== false : true;
  }

  setDoc(doc) {
    this.doc = doc ?? { schema: { tables: [] }, layout: {}, notes: [], groups: [] };
    if (!this.doc.layout) this.doc.layout = {};
    if (!this.doc.notes) this.doc.notes = [];
    if (!this.doc.groups) this.doc.groups = [];
  }

  // setSelection은 지금 고른 것을 정한다.
  //
  // 테이블 키(문자열)와 `{kind, id}` 둘 다 받는다. 구조 화면은 테이블만 고르므로
  // 예전처럼 문자열을 넘기고, 편집기는 간선·메모·그룹까지 고르므로 객체를 넘긴다.
  // 두 화면이 같은 캔버스를 쓰는 이상, 덜 쓰는 쪽에 더 많은 것을 요구하지 않는다.
  setSelection(sel) {
    if (!sel) {
      this.selection = null;
      return;
    }
    this.selection = typeof sel === 'string' ? { kind: 'table', id: sel } : sel;
  }

  // selectedKey는 선택된 테이블 키다(테이블이 아니면 null).
  get selectedKey() {
    return this.selection?.kind === 'table' ? this.selection.id : null;
  }

  isSelected(kind, id) {
    return this.selection?.kind === kind && this.selection.id === id;
  }

  setParticipants(list, youClientId) {
    this.participants = list ?? [];
    this.youClientId = youClientId ?? null;
    // 떠난 사람의 커서를 남겨두면 유령 커서가 화면에 붙어 있는다.
    const alive = new Set(this.participants.map((p) => p.clientId));
    for (const id of [...this.remoteCursors.keys()]) {
      if (!alive.has(id)) this.remoteCursors.delete(id);
    }
  }

  setCursor(clientId, cursor) {
    // cursor가 없으면 지운다. 다른 시점으로 옮겨 간 사람의 커서를 남겨 두면 그
    // 자리에 유령이 붙어 있고, null을 그대로 넣으면 그리는 쪽에서 터진다.
    if (!cursor) this.remoteCursors.delete(clientId);
    else this.remoteCursors.set(clientId, cursor);
    this.renderCursors();
  }

  destroy() {
    this.unbind?.();
    // 화면을 떠나는 순간에도 예약된 프레임이 남아 있을 수 있다.
    // 그 콜백은 이미 버려진 레이어를 만진다.
    if (this.linkFrame) cancelAnimationFrame(this.linkFrame);
    this.linkFrame = 0;
  }

  // ---------- 그리기 ----------

  render() {
    const svg = this.svg;
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    svg.setAttribute('viewBox', `${this.view.x} ${this.view.y} ${this.view.w} ${this.view.h}`);

    const layers = {
      // 그룹은 맨 뒤에 깔린다. 카드 위에 있으면 카드를 고를 수 없다.
      groups: svgEl('g', { class: 'erd-layer-groups' }),
      links: svgEl('g', { class: 'erd-layer-links' }),
      cards: svgEl('g', { class: 'erd-layer-cards' }),
      notes: svgEl('g', { class: 'erd-layer-notes' }),
      cursors: svgEl('g', { class: 'erd-layer-cursors' }),
    };
    svg.appendChild(layers.groups);
    svg.appendChild(layers.links);
    svg.appendChild(layers.notes);
    svg.appendChild(layers.cards);
    svg.appendChild(layers.cursors);
    this.layers = layers;

    const boxes = this.boxes();
    for (const group of this.doc.groups ?? []) layers.groups.appendChild(this.groupEl(group));
    for (const link of this.links(boxes)) layers.links.appendChild(link);
    for (const note of this.doc.notes ?? []) layers.notes.appendChild(this.noteEl(note));
    for (const tbl of this.doc.schema?.tables ?? []) {
      const geom = boxes.get(tableKey(tbl));
      if (geom) layers.cards.appendChild(this.cardEl(tbl, geom));
    }
    this.renderCursors();

    if ((this.doc.schema?.tables ?? []).length === 0 && this.opts.emptyHint) {
      svg.appendChild(svgEl('text', {
        x: this.view.x + this.view.w / 2, y: this.view.y + this.view.h / 2,
        class: 'erd-hint', 'text-anchor': 'middle',
      }, this.opts.emptyHint));
    }
  }

  // boxes는 테이블 키 → 화면 기하 정보를 만든다.
  boxes() {
    const out = new Map();
    for (const tbl of this.doc.schema?.tables ?? []) {
      const key = tableKey(tbl);
      const layout = this.doc.layout?.[key] ?? { x: 80, y: 80 };
      const rows = layout.collapsed ? 0 : Math.min(tbl.columns?.length ?? 0, MAX_VISIBLE_ROWS);
      const extra = !layout.collapsed && (tbl.columns?.length ?? 0) > MAX_VISIBLE_ROWS ? 1 : 0;
      out.set(key, {
        x: layout.x, y: layout.y, w: CARD_W,
        h: HEAD_H + (rows + extra) * ROW_H + (layout.collapsed ? 0 : CARD_PAD),
        rows, extra, layout, table: tbl,
      });
    }
    return out;
  }

  // links는 외래키 관계선을 만든다.
  links(boxes) {
    const out = [];
    for (const tbl of this.doc.schema?.tables ?? []) {
      const from = boxes.get(tableKey(tbl));
      if (!from) continue;
      for (const fk of tbl.foreignKeys ?? []) {
        const to = boxes.get(refKey(tbl, fk));
        if (!to) continue;
        const a = anchor(from, to);
        const b = anchor(to, from);
        // 곡선으로 그리면 여러 관계선이 겹쳐도 구분된다.
        const mx = (a.x + b.x) / 2;
        const d = `M${a.x},${a.y} C${mx},${a.y} ${mx},${b.y} ${b.x},${b.y}`;
        const fkID = `${tableKey(tbl)}.${fk.name}`;

        // 선을 누를 수 있게 만든다. 보이는 선은 1~2px이라 마우스로 맞히기 어려우므로,
        // 같은 경로를 투명한 굵은 선으로 한 겹 더 깔아 그것으로 받는다.
        // 보이는 선을 굵게 하면 그림이 지저분해진다.
        const hit = svgEl('path', { class: 'erd-link-hit', d, 'data-fk': fkID });
        hit.addEventListener('pointerdown', (e) => {
          e.stopPropagation();
          this.selection = { kind: 'link', id: fkID };
          this.opts.onSelectLink?.(fkID);
          this.render();
        });
        out.push(hit);
        out.push(svgEl('path', {
          class: `erd-link${this.isSelected('link', fkID) ? ' is-selected' : ''}`,
          d, 'data-fk': fkID,
        }));
        // 참조하는 쪽(다수)에 점, 참조되는 쪽(하나)에 짧은 막대를 둔다.
        out.push(svgEl('circle', { class: 'erd-link-many', cx: a.x, cy: a.y, r: 3.5 }));
        out.push(svgEl('rect', {
          class: 'erd-link-one', x: b.x - 1, y: b.y - 5, width: 2, height: 10, rx: 1,
        }));
      }
    }
    return out;
  }

  cardEl(tbl, geom) {
    const key = tableKey(tbl);
    const selected = this.isSelected('table', key);
    const g = svgEl('g', {
      class: `erd-card-g${selected ? ' is-selected' : ''}`,
      transform: `translate(${geom.x},${geom.y})`,
      'data-key': key,
      // 색은 **묶음(g)**에 싣는다. 안쪽 사각형에 실으면 그 사각형 자신에게만
      // 정의되어, 머리띠(erd-card-head)처럼 형제 요소는 값을 물려받지 못한다.
      // CSS 규칙도 g를 기준으로 쓰여 있어 아무 일도 일어나지 않았다.
      style: geom.layout.color ? `--card-accent:${geom.layout.color}` : null,
    });

    // 다른 참여자가 선택 중인 테이블은 그 사람의 색으로 테두리를 표시한다.
    const holder = this.participants.find(
      (p) => p.selection === key && p.clientId !== this.youClientId);

    g.appendChild(svgEl('rect', {
      class: 'erd-card-bg', width: geom.w, height: geom.h, rx: 6,
    }));
    if (holder) {
      g.appendChild(svgEl('rect', {
        class: 'erd-card-holder', width: geom.w, height: geom.h, rx: 6,
        stroke: holder.color,
      }));
    }
    g.appendChild(svgEl('rect', { class: 'erd-card-head', width: geom.w, height: HEAD_H, rx: 6 }));

    // 아이콘이 지정되어 있으면 이름 왼쪽에 붙이고 제목을 그만큼 민다.
    const iconName = geom.layout.icon;
    let titleX = 10;
    if (iconName) {
      const mark = icon(iconName, 13);
      mark.setAttribute('x', 9);
      mark.setAttribute('y', 9);
      mark.classList.add('erd-card-icon');
      g.appendChild(mark);
      titleX = 28;
    }
    g.appendChild(svgEl('text', {
      class: 'erd-card-name', x: titleX, y: 20,
    }, truncate(tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name, iconName ? 27 : 30)));

    const count = tbl.columns?.length ?? 0;
    g.appendChild(svgEl('text', {
      class: 'erd-card-count', x: geom.w - 10, y: 20, 'text-anchor': 'end',
    }, `${count}`));

    if (!geom.layout.collapsed) {
      const cols = (tbl.columns ?? []).slice(0, MAX_VISIBLE_ROWS);
      cols.forEach((col, i) => {
        const y = HEAD_H + i * ROW_H + 14;
        const isPK = (tbl.primaryKey?.columns ?? []).some((c) => eqName(c, col.name));
        const isFK = (tbl.foreignKeys ?? []).some((fk) =>
          (fk.columns ?? []).some((c) => eqName(c, col.name)));
        g.appendChild(svgEl('text', {
          class: `erd-col${isPK ? ' is-pk' : ''}`, x: 10, y,
        }, `${isPK ? '● ' : isFK ? '◆ ' : ''}${truncate(col.name, 22)}`));
        g.appendChild(svgEl('text', {
          class: 'erd-col-type', x: geom.w - 10, y, 'text-anchor': 'end',
        }, `${truncate(col.rawType || col.type?.base || '', 16)}${col.nullable ? '' : ' *'}`));
      });
      if (geom.extra) {
        g.appendChild(svgEl('text', {
          class: 'erd-col-more', x: 10, y: HEAD_H + MAX_VISIBLE_ROWS * ROW_H + 14,
        }, `… ${count - MAX_VISIBLE_ROWS}개 더`));
      }
    }

    g.addEventListener('pointerdown', (e) => this.onCardPointerDown(e, key, geom));
    g.addEventListener('dblclick', () => {
      this.opts.onToggleCollapse?.(key, geom);
    });
    return g;
  }

  // groupEl은 테이블 묶음을 감싸는 반투명 사각형이다.
  //
  // 크기를 자동으로 맞추지 않는 이유: 어느 테이블이 이 묶음인지 데이터로 정하지
  // 않았기 때문이다. 사람이 눈으로 묶고 손으로 크기를 정하는 편이 규칙이 없어 단순하다.
  groupEl(group) {
    const g = svgEl('g', {
      class: `erd-group-g${this.isSelected('group', group.id) ? ' is-selected' : ''}`,
      transform: `translate(${group.x},${group.y})`,
      'data-group': group.id,
      style: group.color ? `--group-accent:${group.color}` : null,
    });
    g.appendChild(svgEl('rect', {
      class: 'erd-group-bg', width: group.w || 320, height: group.h || 240, rx: 10,
    }));
    if (group.label) {
      g.appendChild(svgEl('text', { class: 'erd-group-label', x: 12, y: 22 }, group.label));
    }
    if (this.canEdit) {
      const grip = svgEl('rect', {
        class: 'erd-group-grip', x: (group.w || 320) - 16, y: (group.h || 240) - 16,
        width: 12, height: 12, rx: 3,
      });
      grip.addEventListener('pointerdown', (e) => this.onGroupPointerDown(e, group, 'resize'));
      g.appendChild(grip);
    }
    g.addEventListener('pointerdown', (e) => {
      if (e.target.classList.contains('erd-group-grip')) return;
      this.onGroupPointerDown(e, group, 'move');
    });
    g.addEventListener('dblclick', () => this.opts.onEditGroup?.(group));
    return g;
  }

  noteEl(note) {
    const g = svgEl('g', {
      class: `erd-note-g${this.isSelected('note', note.id) ? ' is-selected' : ''}`,
      transform: `translate(${note.x},${note.y})`,
      'data-note': note.id,
      style: note.color ? `--note-accent:${note.color}` : null,
    });
    // 크기는 사람이 정한 값이 있으면 그것을 쓰고, 없으면 글에 맞춘다.
    // 글자 폭은 대략 7px이라, 상자 폭에서 여백을 뺀 만큼이 한 줄에 들어간다.
    const width = note.w || NOTE_W;
    const perLine = Math.max(8, Math.floor((width - 16) / 7));
    const lines = wrapText(note.text || '(빈 메모)', perLine);
    const height = note.h || (16 + lines.length * 16);
    g.appendChild(svgEl('rect', {
      class: 'erd-note-bg', width, height, rx: 4,
    }));
    lines.forEach((line, i) => {
      g.appendChild(svgEl('text', { class: 'erd-note-text', x: 8, y: 18 + i * 16 }, line));
    });
    // 메모도 그룹처럼 손으로 크기를 정한다. 자동으로만 늘어나면 긴 메모가
    // 캔버스를 가로지르고, 그것을 줄일 방법이 없다.
    if (this.canEdit) {
      const grip = svgEl('rect', {
        class: 'erd-note-grip', x: width - 12, y: height - 12,
        width: 9, height: 9, rx: 2,
      });
      grip.addEventListener('pointerdown', (e) => this.onNotePointerDown(e, note, 'resize'));
      g.appendChild(grip);
    }
    g.addEventListener('pointerdown', (e) => this.onNotePointerDown(e, note));
    g.addEventListener('dblclick', () => this.opts.onEditNote?.(note));
    return g;
  }

  renderCursors() {
    if (!this.layers) return;
    const layer = this.layers.cursors;
    while (layer.firstChild) layer.removeChild(layer.firstChild);
    for (const [, c] of this.remoteCursors) {
      const g = svgEl('g', { class: 'erd-cursor', transform: `translate(${c.x},${c.y})` });
      g.appendChild(svgEl('path', {
        d: 'M0,0 L0,14 L4,10 L7,16 L10,14 L7,8 L12,8 Z', fill: c.color,
      }));
      if (c.name) {
        g.appendChild(svgEl('rect', {
          class: 'erd-cursor-label-bg', x: 12, y: 2, width: c.name.length * 8 + 10, height: 16, rx: 3,
          fill: c.color,
        }));
        g.appendChild(svgEl('text', { class: 'erd-cursor-label', x: 17, y: 14 }, c.name));
      }
      layer.appendChild(g);
    }
  }

  // ---------- 상호작용 ----------

  bind() {
    const svg = this.svg;

    const onWheel = (e) => {
      e.preventDefault();
      const p = this.toCanvas(e.clientX, e.clientY);
      this.zoomAt(e.deltaY > 0 ? 1.12 : 0.89, p);
    };
    const onPointerDown = (e) => {
      if (e.target.closest('.erd-card-g') || e.target.closest('.erd-note-g')) return;
      // 빈 곳을 끌면 화면 이동. 선택 해제도 함께 한다.
      this.drag = { mode: 'pan', startClient: { x: e.clientX, y: e.clientY }, view: { ...this.view } };
      // 화면을 손으로 옮기기 시작했다. 따라가기와 두 힘이 동시에 당기면
      // 어느 쪽도 원하는 곳을 볼 수 없다.
      this.opts.onManualPan?.();
      if (this.selection) {
        this.selection = null;
        this.opts.onSelect?.(null);
        this.render();
      }
      svg.setPointerCapture?.(e.pointerId);
    };
    const onPointerMove = (e) => {
      const point = this.toCanvas(e.clientX, e.clientY);
      this.opts.onCursorMove?.({ x: round1(point.x), y: round1(point.y) });

      if (!this.drag) return;
      if (this.drag.mode === 'pan') {
        const scale = this.view.w / svg.clientWidth;
        this.view.x = this.drag.view.x - (e.clientX - this.drag.startClient.x) * scale;
        this.view.y = this.drag.view.y - (e.clientY - this.drag.startClient.y) * scale;
        this.applyViewBox();
        return;
      }
      if (this.drag.mode === 'note-resize') {
        const w = Math.max(120, round1(this.drag.size.w + (point.x - this.drag.start.x)));
        const hh = Math.max(60, round1(this.drag.size.h + (point.y - this.drag.start.y)));
        this.drag.lastSize = { w, h: hh };
        const el = this.dragEl();
        if (!el) return;
        const rect = el.querySelector('.erd-note-bg');
        const grip = el.querySelector('.erd-note-grip');
        if (rect) {
          rect.setAttribute('width', w);
          rect.setAttribute('height', hh);
        }
        if (grip) {
          grip.setAttribute('x', w - 12);
          grip.setAttribute('y', hh - 12);
        }
        return;
      }
      if (this.drag.mode === 'group-resize') {
        const w = Math.max(80, round1(this.drag.size.w + (point.x - this.drag.start.x)));
        const hh = Math.max(60, round1(this.drag.size.h + (point.y - this.drag.start.y)));
        this.drag.lastSize = { w, h: hh };
        const el = this.dragEl();
        if (!el) return;
        const rect = el.querySelector('.erd-group-bg');
        const grip = el.querySelector('.erd-group-grip');
        if (rect) {
          rect.setAttribute('width', w);
          rect.setAttribute('height', hh);
        }
        if (grip) {
          grip.setAttribute('x', w - 16);
          grip.setAttribute('y', hh - 16);
        }
        return;
      }
      if (this.drag.mode === 'card' || this.drag.mode === 'note' || this.drag.mode === 'group') {
        const nx = round1(point.x - this.drag.grab.x);
        const ny = round1(point.y - this.drag.grab.y);
        this.drag.last = { x: nx, y: ny };
        // 드래그 중에는 저장을 기다리지 않고 transform만 바꾼다.
        this.dragEl()?.setAttribute('transform', `translate(${nx},${ny})`);
        // 관계선은 두 카드의 위치에서 계산되므로 카드만 옮기면 선이 제자리에 남는다.
        // 카드가 선에서 떨어져 나온 그림은 "연결이 끊겼나"로 읽힌다.
        if (this.drag.mode === 'card') this.moveLinks(this.drag.key, nx, ny);
      }
    };
    const onPointerUp = () => {
      const drag = this.drag;
      this.drag = null;
      if (drag?.mode === 'note-resize' && drag.lastSize) {
        const note = (this.doc.notes ?? []).find((n) => n.id === drag.id);
        if (note) {
          note.w = drag.lastSize.w;
          note.h = drag.lastSize.h;
        }
        this.opts.onNoteResize?.(drag.id, drag.lastSize.w, drag.lastSize.h);
        this.render();
        return;
      }
      if (drag?.mode === 'group-resize' && drag.lastSize) {
        const group = (this.doc.groups ?? []).find((x) => x.id === drag.id);
        if (group) {
          group.w = drag.lastSize.w;
          group.h = drag.lastSize.h;
        }
        this.opts.onGroupResize?.(drag.id, drag.lastSize.w, drag.lastSize.h);
        this.render();
        return;
      }
      if (!drag || !drag.last) return;
      if (drag.mode === 'card') {
        // 로컬 상태를 먼저 갱신해 두어, 저장 결과가 오기 전에 다시 그려도
        // 카드가 원래 자리로 튀지 않게 한다.
        this.doc.layout[drag.key] = {
          ...(this.doc.layout[drag.key] ?? {}), x: drag.last.x, y: drag.last.y,
        };
        this.opts.onTableMove?.(drag.key, drag.last.x, drag.last.y);
        this.render();
      } else if (drag.mode === 'group') {
        const group = (this.doc.groups ?? []).find((x) => x.id === drag.id);
        if (group) {
          group.x = drag.last.x;
          group.y = drag.last.y;
        }
        this.opts.onGroupMove?.(drag.id, drag.last.x, drag.last.y);
        this.render();
      } else if (drag.mode === 'note') {
        const note = (this.doc.notes ?? []).find((n) => n.id === drag.id);
        if (note) {
          note.x = drag.last.x;
          note.y = drag.last.y;
        }
        this.opts.onNoteMove?.(drag.id, drag.last.x, drag.last.y);
        this.render();
      }
    };

    svg.addEventListener('wheel', onWheel, { passive: false });
    svg.addEventListener('pointerdown', onPointerDown);
    window.addEventListener('pointermove', onPointerMove);
    window.addEventListener('pointerup', onPointerUp);

    this.unbind = () => {
      svg.removeEventListener('wheel', onWheel);
      svg.removeEventListener('pointerdown', onPointerDown);
      window.removeEventListener('pointermove', onPointerMove);
      window.removeEventListener('pointerup', onPointerUp);
    };
  }

  // moveLinks는 끌고 있는 카드의 좌표를 반영해 관계선만 다시 그린다.
  //
  // 선 레이어만 갈아 끼우는 이유: 카드까지 다시 그리면 지금 잡고 있는 요소가 버려져
  // 드래그가 끊긴다(onCardPointerDown의 주석과 같은 이유다).
  //
  // 프레임당 한 번으로 묶는다. pointermove는 화면 갱신보다 훨씬 자주 오므로
  // 그대로 두면 같은 그림을 프레임마다 여러 번 만든다.
  moveLinks(key, x, y) {
    if (!this.layers?.links) return;
    const prev = this.doc.layout[key] ?? {};
    this.doc.layout[key] = { ...prev, x, y };
    if (this.linkFrame) return;
    this.linkFrame = requestAnimationFrame(() => {
      this.linkFrame = 0;
      if (!this.layers?.links) return;
      const layer = this.layers.links;
      while (layer.firstChild) layer.removeChild(layer.firstChild);
      for (const link of this.links(this.boxes())) layer.appendChild(link);
    });
  }

  onCardPointerDown(e, key, geom) {
    e.stopPropagation();
    // 선택은 언제나 {kind, id} 형태로 둔다. 여기서만 문자열을 넣으면
    // isSelected가 어긋나 카드에 선택 표시가 그려지지 않는다 — 구조 화면이
    // 그랬다(그쪽은 setSelection을 다시 부르지 않아 문자열이 그대로 남았다).
    this.selection = { kind: 'table', id: key };
    this.opts.onSelect?.(key);
    // 선택 표시를 즉시 반영한다. **다시 그린 뒤에** 드래그 대상을 찾아야 한다 —
    // 지금 손에 쥔 요소는 재렌더로 버려지고, 버려진 요소를 옮기면 화면에서
    // 아무 일도 일어나지 않는다.
    this.render();
    if (!this.canEdit) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const el = this.svg.querySelector(`.erd-card-g[data-key="${cssEscape(key)}"]`);
    if (!el) return;
    this.drag = {
      mode: 'card', key, el,
      selector: `.erd-card-g[data-key="${cssEscape(key)}"]`,
      grab: { x: p.x - geom.x, y: p.y - geom.y },
    };
  }

  onNotePointerDown(e, note, mode = 'move') {
    e.stopPropagation();
    // 고르는 것과 옮기는 것은 다른 권한이다. 읽기 전용 참여자도 메모를 골라
    // 내용을 인스펙터에서 읽을 수 있어야 한다.
    this.selection = { kind: 'note', id: note.id };
    this.opts.onSelectNote?.(note.id);
    this.render();
    if (!this.canEdit) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const el = this.svg.querySelector(`.erd-note-g[data-note="${cssEscape(note.id)}"]`);
    if (!el) return;
    const selector = `.erd-note-g[data-note="${cssEscape(note.id)}"]`;
    this.drag = mode === 'resize'
      ? {
        mode: 'note-resize', id: note.id, el, selector, note,
        start: p, size: { w: note.w || NOTE_W, h: note.h || noteHeight(note) },
      }
      : {
        mode: 'note', id: note.id, el, selector,
        grab: { x: p.x - note.x, y: p.y - note.y },
      };
  }

  onGroupPointerDown(e, group, mode) {
    e.stopPropagation();
    this.selection = { kind: 'group', id: group.id };
    this.opts.onSelectGroup?.(group.id);
    this.render();
    if (!this.canEdit) return;
    const p = this.toCanvas(e.clientX, e.clientY);
    const el = this.svg.querySelector(`.erd-group-g[data-group="${cssEscape(group.id)}"]`);
    if (!el) return;
    const selector = `.erd-group-g[data-group="${cssEscape(group.id)}"]`;
    this.drag = mode === 'resize'
      ? {
        mode: 'group-resize', id: group.id, el, selector, group,
        start: p, size: { w: group.w || 320, h: group.h || 240 },
      }
      : { mode: 'group', id: group.id, el, selector, grab: { x: p.x - group.x, y: p.y - group.y } };
  }

  // dragEl은 지금 끌고 있는 요소를 **살아 있는 것으로** 돌려준다.
  //
  // 누르는 순간 잡아 둔 요소는 곧 버려질 수 있다. 선택이 바뀌면 화면 쪽에서
  // 다시 그리기 때문이다(onSelect → renderCanvas). 버려진 요소를 옮기면 아무 일도
  // 일어나지 않는데, 관계선은 매번 새로 찾으므로 선만 따라 움직인다 —
  // "고르지 않은 테이블을 끌면 간선만 움직인다"가 그 증상이었다.
  dragEl() {
    const d = this.drag;
    if (!d) return null;
    if (d.el?.isConnected) return d.el;
    if (!d.selector) return null;
    d.el = this.svg.querySelector(d.selector);
    return d.el;
  }

  // ---------- 뷰포트 ----------

  applyViewBox() {
    this.svg.setAttribute('viewBox',
      `${this.view.x} ${this.view.y} ${this.view.w} ${this.view.h}`);
  }

  toCanvas(clientX, clientY) {
    const ctm = this.svg.getScreenCTM();
    if (!ctm) return { x: 0, y: 0 };
    const pt = this.svg.createSVGPoint();
    pt.x = clientX;
    pt.y = clientY;
    const p = pt.matrixTransform(ctm.inverse());
    return { x: p.x, y: p.y };
  }

  zoom(factor) {
    this.zoomAt(factor, {
      x: this.view.x + this.view.w / 2,
      y: this.view.y + this.view.h / 2,
    });
  }

  // centerOn은 캔버스 좌표 한 점을 화면 가운데로 옮긴다.
  //
  // 확대율은 건드리지 않는다. 따라가기에서 배율까지 맞추면 상대가 줌을 만질 때마다
  // 내 화면이 튀고, 그러면 무엇을 보고 있는지가 오히려 흐려진다.
  centerOn(x, y) {
    this.view.x = x - this.view.w / 2;
    this.view.y = y - this.view.h / 2;
    this.applyViewBox();
  }

  // zoomAt은 지정한 캔버스 좌표를 고정한 채 확대/축소한다.
  // 화면 중앙 기준으로만 확대하면 마우스가 가리키는 곳이 화면에서 벗어난다.
  zoomAt(factor, focus) {
    const nw = clamp(this.view.w * factor, 200, 12000);
    const nh = this.view.h * (nw / this.view.w);
    this.view.x = focus.x - (focus.x - this.view.x) * (nw / this.view.w);
    this.view.y = focus.y - (focus.y - this.view.y) * (nh / this.view.h);
    this.view.w = nw;
    this.view.h = nh;
    this.applyViewBox();
  }

  // fitView는 모든 요소가 보이도록 뷰포트를 맞춘다.
  fitView() {
    const aspect = this.wrap.clientHeight > 0
      ? this.wrap.clientWidth / this.wrap.clientHeight : 1.5;
    const boxes = [...this.boxes().values()];
    const notes = this.doc.notes ?? [];
    if (boxes.length === 0 && notes.length === 0) {
      this.view = { x: 0, y: 0, w: 1200, h: 1200 / aspect };
      return;
    }
    let minX = Infinity; let minY = Infinity; let maxX = -Infinity; let maxY = -Infinity;
    for (const b of boxes) {
      minX = Math.min(minX, b.x); minY = Math.min(minY, b.y);
      maxX = Math.max(maxX, b.x + b.w); maxY = Math.max(maxY, b.y + b.h);
    }
    for (const n of notes) {
      minX = Math.min(minX, n.x); minY = Math.min(minY, n.y);
      maxX = Math.max(maxX, n.x + 200); maxY = Math.max(maxY, n.y + 80);
    }
    const pad = 60;
    const w = Math.max(maxX - minX + pad * 2, 400);
    const hh = Math.max(maxY - minY + pad * 2, 300);
    // 가로세로 비율을 캔버스에 맞춰야 preserveAspectRatio 때문에 여백이 치우치지 않는다.
    const fitW = Math.max(w, hh * aspect);
    const fitH = fitW / aspect;
    this.view = {
      x: minX - pad - (fitW - w) / 2,
      y: minY - pad - (fitH - hh) / 2,
      w: fitW, h: fitH,
    };
  }

  // 화면 중앙의 캔버스 좌표. 새 요소를 "지금 보고 있는 곳"에 놓기 위해 쓴다.
  center(offsetX = 0, offsetY = 0) {
    return {
      x: round1(this.view.x + this.view.w / 2 - offsetX),
      y: round1(this.view.y + this.view.h / 2 - offsetY),
    };
  }
}

// ---------- 공용 헬퍼 ----------

// NOTE_W는 메모의 기본 폭이다. 크기를 정하지 않은 메모가 이 폭으로 그려진다.
export const NOTE_W = 200;

// noteHeight는 크기를 정하지 않은 메모의 높이다(글 줄 수에 맞춘다).
export function noteHeight(note) {
  const perLine = Math.max(8, Math.floor(((note.w || NOTE_W) - 16) / 7));
  return 16 + wrapText(note.text || '(빈 메모)', perLine).length * 16;
}

export function tableKey(tbl) {
  return (tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name).toLowerCase();
}

export function tableDisplay(tbl) {
  return tbl.namespace ? `${tbl.namespace}.${tbl.name}` : tbl.name;
}

export function refKey(tbl, fk) {
  const ns = fk.refNamespace || tbl.namespace || '';
  return (ns ? `${ns}.${fk.refTable}` : fk.refTable).toLowerCase();
}

export function eqName(a, b) {
  return String(a).toLowerCase() === String(b).toLowerCase();
}

// anchor는 from 카드에서 to 카드를 향하는 변의 접점을 고른다.
function anchor(from, to) {
  const fc = { x: from.x + from.w / 2, y: from.y + from.h / 2 };
  const tc = { x: to.x + to.w / 2, y: to.y + to.h / 2 };
  if (Math.abs(tc.x - fc.x) > Math.abs(tc.y - fc.y)) {
    return { x: tc.x > fc.x ? from.x + from.w : from.x, y: fc.y };
  }
  return { x: fc.x, y: tc.y > fc.y ? from.y + from.h : from.y };
}

export function svgEl(tag, attrs = {}, text) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    el.setAttribute(k, v);
  }
  if (text !== undefined) el.textContent = text;
  return el;
}

export function truncate(text, max) {
  const s = String(text ?? '');
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

function wrapText(text, width) {
  const out = [];
  for (const para of String(text).split('\n')) {
    let line = '';
    for (const word of para.split(/\s+/)) {
      if (!line) {
        line = word;
      } else if ((line + ' ' + word).length <= width) {
        line += ` ${word}`;
      } else {
        out.push(line);
        line = word;
      }
      if (out.length >= 8) return out;
    }
    out.push(line);
  }
  return out.slice(0, 8);
}

function clamp(v, min, max) {
  return Math.min(Math.max(v, min), max);
}

export function round1(v) {
  return Math.round(v * 10) / 10;
}

// cssEscape는 속성 선택자에 쓸 값을 안전하게 만든다.
// 테이블 키에는 점과 따옴표가 들어갈 수 있다.
function cssEscape(value) {
  return String(value).replace(/["\\]/g, '\\$&');
}

// newLocalID는 메모·그룹처럼 클라이언트가 만드는 요소의 식별자다.
export function newLocalID(prefix) {
  return `${prefix}-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 6)}`;
}

// 캔버스 위에 아무것도 없을 때 쓰는 안내 요소.
export function canvasWrap() {
  return h('div.erd-canvas-wrap');
}

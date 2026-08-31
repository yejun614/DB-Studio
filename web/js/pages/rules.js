// 감시 룰 설정 화면.
import { api } from '../core/api.js';
import { withProject } from '../core/project.js';
import {
  h, mount, icon, field, input, select, textarea, checkbox, spinner,
  emptyState, pageHeader, badge, envBadge, openModal, confirmDialog,
  toast, toastError,
} from '../core/ui.js';
import { errorPanel } from './users.js';
import { serverDbPicker } from '../core/connpick.js';

const OPS = [
  { value: '>', label: '초과 (>)' },
  { value: '>=', label: '이상 (>=)' },
  { value: '<', label: '미만 (<)' },
  { value: '<=', label: '이하 (<=)' },
  { value: '==', label: '같음 (==)' },
  { value: '!=', label: '다름 (!=)' },
];

const KINDS = [
  { value: 'threshold', label: '임계치 감시' },
  { value: 'connectivity', label: '접속 실패 감지' },
  { value: 'drift', label: '스키마 외부 변경 감지' },
];

const SEVERITIES = [
  { value: 'info', label: '정보' },
  { value: 'warning', label: '경고' },
  { value: 'critical', label: '심각' },
];

export async function renderRules(outlet) {
  mount(outlet, spinner('룰을 불러오는 중…'));

  let data;
  let conns;
  try {
    [data, conns] = await Promise.all([
      api.get('/monitor/rules'),
      api.get(withProject('/connections/')),
    ]);
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const connNames = {};
  for (const i of conns.items) connNames[i.connection.id] = i.connection.name;

  const reload = () => renderRules(outlet);

  mount(outlet,
    pageHeader('감시 룰', `${data.rules.length}개 · 위반이 지속되면 이벤트를 만듭니다`, [
      h('a.btn', { href: '/monitor' }, '← 모니터링'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: () => openRuleForm(null, data.metrics, conns.items, reload),
      }, icon('plus'), '룰 추가'),
    ]),
    h('p.notice.notice-info', {}, icon('activity'),
      '지속 시간을 두면 순간적인 스파이크로 이벤트가 쏟아지는 것을 막을 수 있습니다. ' +
      '해당 DB가 제공하지 않는 지표의 룰은 조용히 건너뜁니다.'),
    data.rules.length === 0
      ? emptyState('감시 룰이 없습니다')
      : h('div.card', {}, ruleTable(data, connNames, conns.items, reload)),
  );
}

function ruleTable(data, connNames, connItems, reload) {
  return h('table.table.rule-table', {},
    h('thead', {}, h('tr', {},
      h('th', {}, '이름'),
      h('th', {}, '종류'),
      h('th', {}, '조건'),
      h('th', {}, '적용 대상'),
      h('th', {}, '심각도'),
      h('th', {}, '상태'),
      h('th.col-actions', {}, ''),
    )),
    h('tbody', {}, data.rules.map((entry) =>
      ruleRow(entry, data.metrics, connNames, connItems, reload))),
  );
}

function ruleRow({ rule, describe }, metrics, connNames, connItems, reload) {
  const scope = rule.connectionId
    ? connNames[rule.connectionId] ?? '(삭제된 커넥션)'
    : rule.environment
      ? null
      : '모든 커넥션';

  const toggle = h('input', {
    type: 'checkbox', checked: rule.enabled,
    onchange: async (e) => {
      try {
        await api.put(`/monitor/rules/${rule.id}`, { ...toPayload(rule), enabled: e.target.checked });
        toast(e.target.checked ? '룰을 활성화했습니다' : '룰을 비활성화했습니다', 'success', 2000);
      } catch (err) {
        e.target.checked = !e.target.checked;
        toastError(err);
      }
    },
  });

  return h('tr', { class: rule.enabled ? '' : 'row-muted' },
    h('td', {},
      h('div.cell-main', {}, rule.name, rule.builtin ? badge('기본', 'neutral') : null),
      rule.description ? h('div.cell-sub', {}, rule.description) : null,
    ),
    h('td', {}, badge(kindLabelOf(rule.kind), kindTone(rule.kind))),
    h('td', {}, rule.kind === 'threshold'
      ? h('code.rule-condition', {}, describe)
      : h('span.muted', {}, rule.durationSec ? `${rule.durationSec}초 지속 시` : '즉시')),
    h('td', {},
      scope ? h('span', {}, scope) : null,
      rule.environment ? envBadge(rule.environment) : null,
    ),
    h('td', {}, severityBadge(rule.severity)),
    h('td', {}, h('label.checkbox', {}, toggle, h('span', {}, rule.enabled ? '켜짐' : '꺼짐'))),
    h('td.col-actions', {},
      h('div.row-actions', {},
        h('button.icon-btn', {
          type: 'button', title: '수정',
          onclick: () => openRuleForm(rule, metrics, connItems, reload),
        }, icon('edit')),
        h('button.icon-btn.danger', {
          type: 'button', title: '삭제',
          onclick: async () => {
            const ok = await confirmDialog({
              title: '룰 삭제',
              message: `"${rule.name}" 룰을 삭제합니다. 이 룰이 만든 열린 이벤트는 해소됩니다.`,
              confirmLabel: '삭제', danger: true,
            });
            if (!ok) return;
            try {
              await api.del(`/monitor/rules/${rule.id}`);
              toast('룰을 삭제했습니다', 'success');
              reload();
            } catch (err) { toastError(err); }
          },
        }, icon('trash')),
      ),
    ),
  );
}

function toPayload(rule) {
  return {
    name: rule.name, connectionId: rule.connectionId ?? '',
    environment: rule.environment ?? '', kind: rule.kind,
    metric: rule.metric, op: rule.op, threshold: rule.threshold,
    durationSec: rule.durationSec, severity: rule.severity,
    description: rule.description ?? '',
  };
}

function openRuleForm(existing, metrics, connItems, reload) {
  const isEdit = Boolean(existing);

  const name = input({ value: existing?.name ?? '', placeholder: '세션 사용률 경고' });
  const kind = select(KINDS, { value: existing?.kind ?? 'threshold' });
  const metricSelect = select(
    metrics.map((m) => ({ value: m.name, label: `${m.label} (${m.name})` })),
    { value: existing?.metric ?? metrics[0]?.name ?? '' },
  );
  const op = select(OPS, { value: existing?.op || '>' });
  const threshold = input({ type: 'number', step: 'any', value: existing?.threshold ?? 80 });
  const duration = input({ type: 'number', min: '0', value: existing?.durationSec ?? 60 });
  const severity = select(SEVERITIES, { value: existing?.severity ?? 'warning' });
  const connection = serverDbPicker({
    usable: connItems,
    currentId: existing?.connectionId ?? '',
    onPick: () => {},
    serverLabel: '적용 서버',
    allLabel: '모든 커넥션',
    serverHelp: '전체 적용은 관리자만 가능합니다',
    inline: false,
  });
  const environment = select(
    [{ value: '', label: '모든 환경' }, { value: 'dev', label: '개발만' }, { value: 'prod', label: '운영만' }],
    { value: existing?.environment ?? '' },
  );
  const description = textarea({ value: existing?.description ?? '', placeholder: '이 룰이 무엇을 감지하는지' });
  const enabled = checkbox('활성화', { checked: existing?.enabled ?? true });

  // 지표 설명과 단위를 실시간으로 보여줘 임계값을 감으로 넣지 않게 한다.
  const metricHelp = h('span.field-help');
  const thresholdUnit = h('span.input-suffix');
  const syncMetric = () => {
    const m = metrics.find((x) => x.name === metricSelect.value);
    metricHelp.textContent = m?.help ?? '';
    thresholdUnit.textContent = unitSuffix(m?.unit);
  };
  metricSelect.addEventListener('change', syncMetric);
  syncMetric();

  // 종류에 따라 임계치 관련 필드를 감춘다.
  const thresholdFields = h('div.form-grid');
  const syncKind = () => {
    const isThreshold = kind.value === 'threshold';
    thresholdFields.style.display = isThreshold ? '' : 'none';
    metricHelp.style.display = isThreshold ? '' : 'none';
  };
  kind.addEventListener('change', syncKind);

  mount(thresholdFields,
    h('label.field', {},
      h('span.field-label', {}, '감시 지표'), metricSelect, metricHelp),
    h('div.form-grid', {},
      field('조건', op),
      h('label.field', {},
        h('span.field-label', {}, '임계값'),
        h('div.input-with-suffix', {}, threshold, thresholdUnit)),
    ),
  );
  syncKind();

  const submit = async (close) => {
    const payload = {
      name: name.value.trim(),
      kind: kind.value,
      metric: kind.value === 'threshold' ? metricSelect.value : '',
      op: kind.value === 'threshold' ? op.value : '',
      threshold: Number(threshold.value) || 0,
      durationSec: Number(duration.value) || 0,
      severity: severity.value,
      connectionId: connection.value,
      environment: environment.value,
      description: description.value.trim(),
      enabled: enabled.querySelector('input').checked,
    };
    try {
      if (isEdit) {
        await api.put(`/monitor/rules/${existing.id}`, payload);
        toast('룰을 수정했습니다', 'success');
      } else {
        await api.post('/monitor/rules', payload);
        toast('룰을 추가했습니다', 'success');
      }
      close();
      reload();
    } catch (err) {
      toastError(err);
    }
  };

  openModal({
    title: isEdit ? `룰 수정 — ${existing.name}` : '룰 추가',
    width: 660,
    body: () => [
      field('이름', name),
      field('종류', kind, '접속/스키마 감지는 지표와 임계값을 쓰지 않습니다'),
      thresholdFields,
      h('div.form-grid', {},
        h('label.field', {},
          h('span.field-label', {}, '지속 시간 (초)'), duration,
          h('span.field-help', {}, '0이면 즉시 발동. 값을 주면 그 시간 동안 연속 위반해야 이벤트가 생깁니다')),
        field('심각도', severity),
      ),
      h('div.form-grid', {},
        ...connection.nodes,
        field('적용 환경', environment, '운영에만 더 엄격한 임계치를 둘 수 있습니다'),
      ),
      field('설명', description),
      h('div.field', {}, h('span.field-label', {}, ' '), enabled),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', { type: 'button', onclick: () => submit(close) },
        isEdit ? '저장' : '추가'),
    ],
  });
}

function unitSuffix(unit) {
  return {
    percent: '%', bytes: 'bytes', ms: 'ms', s: '초', per_sec: '/초',
    count: '개', ratio: '배',
  }[unit] ?? '';
}

function kindLabelOf(kind) {
  return KINDS.find((k) => k.value === kind)?.label ?? kind;
}

function kindTone(kind) {
  return { threshold: 'info', connectivity: 'danger', drift: 'accent' }[kind] ?? 'neutral';
}

function severityBadge(severity) {
  const map = { critical: ['심각', 'danger'], warning: ['경고', 'warn'], info: ['정보', 'info'] };
  const [label, tone] = map[severity] ?? [severity, 'neutral'];
  return badge(label, tone);
}

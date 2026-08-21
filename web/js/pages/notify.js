// 알림 설정 화면 (슈퍼 어드민 전용).
//
// 이벤트를 메신저(Mattermost·Slack)로 내보내는 규칙을 정한다. 화면이 답해야 하는 질문은
// 셋이다: 어디로 보내는가, 무엇을 보내는가, 지금 잘 가고 있는가. 마지막 하나가 없으면
// "설정은 했는데 안 온다"를 확인할 방법이 서버 로그밖에 남지 않는다.
import { api } from '../core/api.js';
import {
  h, mount, icon, input, select, checkbox, field, spinner, badge,
  pageHeader, toast, toastError, formatDate, relativeTime,
} from '../core/ui.js';
import { errorPanel } from './users.js';

export async function renderNotify(outlet) {
  mount(outlet, spinner('알림 설정을 불러오는 중…'));

  let data;
  try {
    data = await api.get('/notify/');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return;
  }

  const cfg = data.settings ?? {};
  const reload = () => renderNotify(outlet);

  // 웹훅 주소는 마스킹된 값이 온다. 그 값을 그대로 저장하면 마스킹 문자열이
  // 주소가 되어 버리므로, **사용자가 새로 입력했을 때만** 서버로 보낸다.
  const providers = data.providers ?? [];
  const providerSelect = select(
    providers.map((p) => ({ value: p.value, label: p.label })),
    { value: cfg.provider || 'mattermost' },
  );
  const providerNote = h('span.field-help');
  const webhookInput = input({ value: '' });

  // 메신저마다 주소 모양과 지켜야 할 것이 다르다. 고르면 안내와 예시가 함께 바뀐다 —
  // "채널을 적었는데 다른 곳으로 간다"(Slack 앱 웹훅)를 겪고 나서 찾아보게 두지 않는다.
  const syncProvider = () => {
    const cur = providers.find((p) => p.value === providerSelect.value);
    providerNote.textContent = cur?.note ?? '';
    const slack = providerSelect.value === 'slack';
    channelHelp.textContent = slack
      ? '앱 웹훅에서는 무시됩니다(웹훅을 만들 때 고른 채널로 갑니다). 예전 방식의 커스텀 웹훅에서만 반영됩니다.'
      : '웹훅에 설정된 기본 채널 대신 보낼 채널입니다.';
    usernameHelp.textContent = slack
      ? '앱 웹훅에서는 무시됩니다. 앱 이름으로 표시됩니다.'
      : '메시지에 표시할 이름입니다.';
    webhookInput.placeholder = cfg.webhookUrl || (providerSelect.value === 'slack'
      ? 'https://hooks.slack.com/services/T000/B000/xxxxxxxx'
      : 'https://mattermost.example.com/hooks/xxxxxxxx');
  };
  providerSelect.addEventListener('change', syncProvider);
  const channelInput = input({ value: cfg.channel ?? '', placeholder: '예: db-alerts (비우면 웹훅 기본 채널)' });
  const usernameInput = input({ value: cfg.username ?? '', placeholder: 'DB Studio' });
  // 채널·보내는 이름은 메신저에 따라 효력이 다르다. 같은 설명을 두면 Slack에서
  // "적었는데 반영이 안 된다"가 되고, 그 이유를 화면 어디에서도 찾을 수 없다.
  const channelHelp = h('span.field-help');
  const usernameHelp = h('span.field-help');
  const appURLInput = input({ value: cfg.appUrl ?? '', placeholder: 'https://db.example.com' });
  const enabledBox = checkbox('알림 보내기', { checked: Boolean(cfg.enabled) });
  const resolvedBox = checkbox('이벤트가 해소될 때도 알리기', { checked: cfg.includeResolved !== false });
  const severitySelect = select([
    { value: 'info', label: '정보 이상 (모두)' },
    { value: 'warning', label: '경고 이상 (권장)' },
    { value: 'critical', label: '심각만' },
  ], { value: cfg.minSeverity || 'warning' });

  // 종류를 하나도 고르지 않으면 "전체"다. 목록은 서버가 함께 준다 —
  // 화면이 따로 들고 있으면 이벤트 종류가 늘 때 조용히 빠진다.
  const kindBoxes = (data.kinds ?? []).map((k) => {
    const box = checkbox(k.label, { checked: (cfg.kinds ?? []).includes(k.value) });
    box.dataset.kind = k.value;
    return box;
  });

  const save = h('button.btn.btn-primary', { type: 'button' }, icon('save'), '저장');
  const test = h('button.btn', { type: 'button' }, icon('play'), '테스트 전송');

  mount(outlet,
    pageHeader('알림', '모니터링 이벤트를 Mattermost·Slack 채널로 보냅니다'),
    h('div.card', {},
      h('div.card-title', {},
        h('span', {}, '연결'),
        cfg.enabled ? badge('켜짐', 'success') : badge('꺼짐', 'neutral'),
      ),
      field('메신저', providerSelect),
      providerNote,
      h('p.field-help', {},
        '들어오는 웹훅(incoming webhook) 주소를 넣습니다. '
        + '이 주소는 그것 하나로 해당 채널에 글을 쓸 수 있는 비밀이므로 암호화해 저장하고, '
        + '이 화면에는 일부만 보여줍니다.'),
      field('웹훅 주소', webhookInput,
        cfg.webhookUrl ? `지금 저장된 주소: ${cfg.webhookUrl} (바꿀 때만 입력하세요)` : '아직 저장된 주소가 없습니다'),
      h('div.form-grid', {},
        h('label.field', {}, h('span.field-label', {}, '채널'), channelInput, channelHelp),
        h('label.field', {}, h('span.field-label', {}, '보내는 이름'), usernameInput, usernameHelp),
      ),
      field('이 앱의 주소', appURLInput,
        '알림에 "이벤트 화면에서 보기" 링크를 붙입니다. 비우면 링크 없이 보냅니다.'),
    ),

    h('div.card', {},
      h('h2.card-title', {}, '무엇을 보낼지'),
      enabledBox,
      field('최소 심각도', severitySelect,
        '이보다 낮은 이벤트는 보내지 않습니다. 정보까지 보내면 채널이 금세 시끄러워집니다.'),
      h('div.field', {},
        h('span.field-label', {}, '이벤트 종류'),
        h('div.notify-kinds', {}, kindBoxes),
        h('span.field-help', {}, '아무것도 고르지 않으면 모든 종류를 보냅니다.'),
      ),
      resolvedBox,
      h('div.notify-actions', {}, save, test),
    ),

    statusCard(data.status),
  );

  syncProvider();

  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      const body = {
        enabled: enabledBox.querySelector('input').checked,
        provider: providerSelect.value,
        channel: channelInput.value,
        username: usernameInput.value,
        appUrl: appURLInput.value,
        minSeverity: severitySelect.value,
        includeResolved: resolvedBox.querySelector('input').checked,
        kinds: kindBoxes
          .filter((b) => b.querySelector('input').checked)
          .map((b) => b.dataset.kind),
      };
      // 빈 칸은 "바꾸지 않음"이다. 보내면 저장된 주소가 지워진다.
      const next = webhookInput.value.trim();
      if (next) body.webhookUrl = next;

      await api.put('/notify/', body);
      toast('알림 설정을 저장했습니다', 'success');
      reload();
    } catch (err) {
      toastError(err);
      save.disabled = false;
    }
  });

  test.addEventListener('click', async () => {
    test.disabled = true;
    try {
      // 저장된 설정으로 보낸다. 화면의 값으로 보내면 "테스트는 됐는데 실제로는
      // 안 온다"가 가능해진다.
      await api.post('/notify/test');
      toast('테스트 메시지를 보냈습니다. 채널을 확인하세요', 'success');
      reload();
    } catch (err) {
      toastError(err);
      test.disabled = false;
    }
  });
}

// statusCard는 마지막 전송 결과다. "설정은 했는데 안 온다"에 답하는 자리다.
function statusCard(status) {
  if (!status) {
    return h('div.card', {},
      h('h2.card-title', {}, '전송 상태'),
      h('p.muted', {}, '전송기가 꺼져 있습니다.'));
  }
  if (!status.at) {
    return h('div.card', {},
      h('h2.card-title', {}, '전송 상태'),
      h('p.muted', {}, '아직 보낸 알림이 없습니다. [테스트 전송]으로 확인해 보세요.'));
  }
  return h('div.card', {},
    h('div.card-title', {},
      h('span', {}, '전송 상태'),
      status.ok ? badge('정상', 'success') : badge('실패', 'danger'),
    ),
    h('dl.mig-meta', {},
      h('div.meta-row', {}, h('dt', {}, '마지막 전송'),
        h('dd', {}, `${formatDate(status.at)} (${relativeTime(status.at)})`)),
      status.detail
        ? h('div.meta-row', {}, h('dt', {}, '사유'), h('dd', {}, status.detail))
        : null,
      status.dropped
        ? h('div.meta-row', {}, h('dt', {}, '버린 알림'),
          h('dd', {}, `${status.dropped}건 (메신저가 오래 응답하지 않아 큐가 찼습니다)`))
        : null,
    ),
  );
}

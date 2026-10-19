// 클라우드 연결 화면 (슈퍼 어드민 전용).
//
// DB Studio 자신의 데이터(메타 DB)를 두 곳에 잇는다.
//   - Cloudflare D1: 변경을 모아 두었다가 주기적으로 반영한다. 새 환경에서 같은 D1을
//     연결해 가져오면 그 데이터로 그대로 운영한다.
//   - R2·S3: 메타 DB를 통째로 떠서 올리고, 고른 백업으로 되돌린다.
//
// 화면이 답할 질문은 셋이다: 지금 어디에 이어져 있는가, 잘 따라가고 있는가(밀린 변경·마지막
// 오류), 그리고 되돌릴 수 없는 일(가져오기·덮어쓰기·복구)을 누르기 전에 무엇이 바뀌는가.
import { api } from '../core/api.js';
import {
  h, mount, icon, input, select, checkbox, field, spinner, badge,
  pageHeader, toast, toastError, formatDate, relativeTime, confirmDialog,
} from '../core/ui.js';
import { errorPanel } from './users.js';

const D1_STATE = {
  off: ['꺼짐', 'neutral'],
  ok: ['동기화 중', 'success'],
  setup: ['선택 필요', 'warn'],
  conflict: ['주인이 바뀜', 'danger'],
  error: ['오류', 'danger'],
  replica: ['리플리카', 'neutral'],
};

const PROVIDERS = [
  { value: 'r2', label: 'Cloudflare R2' },
  { value: 's3', label: 'AWS S3' },
  { value: 'custom', label: 'S3 호환 (MinIO·Ceph RGW·Wasabi 등)' },
];

const EVERY = [1, 3, 6, 12, 24, 48, 72, 168].map((n) => ({
  value: String(n),
  label: n < 24 ? `${n}시간마다` : n === 24 ? '매일' : n === 168 ? '매주' : `${n / 24}일마다`,
}));

export async function renderCloud(outlet) {
  mount(outlet, spinner('클라우드 연결을 불러오는 중…'));
  let data;
  try {
    data = await api.get('/cloud/');
  } catch (err) {
    mount(outlet, errorPanel(err));
    return null;
  }

  const reload = () => renderCloud(outlet);
  const status = data.status ?? {};
  const replica = status.role === 'replica';

  // 상태가 바뀌는 자리만 따로 들고 있다가 갈아 끼운다. 화면 전체를 다시 그리면 입력하던
  // 칸이 지워진다(작업이 도는 동안 몇 초마다 상태를 다시 읽는다).
  const jobBox = h('div');
  const d1StatusBox = h('div');
  const objStatusBox = h('div');
  const backupsBox = h('div');

  const d1 = d1Card(data, d1StatusBox, reload);
  const obj = objectCard(data, objStatusBox, reload);

  mount(outlet,
    pageHeader('클라우드 연결',
      'DB Studio 자신의 데이터(커넥션·권한·ERD·매크로·감사 기록)를 Cloudflare D1 에 동기화하고 R2·S3 에 백업합니다'),
    replica
      ? h('p.notice.notice-warn', {}, icon('alert'),
        '이 노드는 클러스터 리플리카입니다. 동기화와 백업은 마스터가 합니다. 여기서 저장한 설정은 마스터로 넘어갑니다.')
      : null,
    jobBox,
    d1,
    obj,
    backupsBox,
  );

  let lastSeenJob = status.job ? status.job.startedAt : null;
  const paint = (st) => {
    mount(jobBox, jobCard(st));
    mount(d1StatusBox, d1Status(st.d1 ?? {}, data.d1 ?? {}));
    mount(objStatusBox, objectStatus(st.object ?? {}));
  };
  paint(status);
  if (data.object?.bucket) loadBackups(backupsBox);

  // 작업이 도는 동안은 자주, 아니면 가끔 상태를 읽는다.
  let timer = null;
  let stopped = false;
  const poll = async () => {
    if (stopped) return;
    let st;
    try {
      st = (await api.get('/cloud/')).status ?? {};
    } catch {
      timer = setTimeout(poll, 5000);
      return;
    }
    if (stopped) return;
    paint(st);
    const running = st.job;
    const done = st.lastJob;
    // 지켜보던 작업이 끝났다.
    if (!running && lastSeenJob && done && done.startedAt === lastSeenJob) {
      lastSeenJob = null;
      if (done.error) {
        toast(`${done.label} 실패: ${done.error}`, 'error', 8000);
      } else if (done.relogin) {
        // 메타 DB를 통째로 바꿨다. 지금 세션과 화면의 모든 값이 옛것이다.
        await confirmDialog({
          title: '데이터를 바꿨습니다',
          message: `${done.result} 지금 들고 있는 화면과 로그인은 바뀌기 전의 것입니다. 새로 엽니다.`,
          confirmLabel: '새로 열기',
        });
        location.assign('/');
        return;
      } else {
        toast(done.result || `${done.label} 완료`, 'success');
        if (done.kind === 'object.backup') loadBackups(backupsBox);
      }
    }
    if (running) lastSeenJob = running.startedAt;
    timer = setTimeout(poll, running ? 1500 : 10000);
  };
  timer = setTimeout(poll, status.job ? 1500 : 10000);

  // 작업을 시작한 쪽이 곧바로 지켜보게 한다.
  const onJob = (e) => {
    lastSeenJob = e.detail?.startedAt ?? lastSeenJob;
    clearTimeout(timer);
    timer = setTimeout(poll, 500);
  };
  outlet.addEventListener('cloud-job', onJob);

  return () => {
    stopped = true;
    clearTimeout(timer);
    outlet.removeEventListener('cloud-job', onJob);
  };
}

// startJob은 배경 작업을 시작하고 화면에 알린다.
async function startJob(btn, path, body) {
  btn.disabled = true;
  try {
    const res = await api.post(path, body);
    btn.dispatchEvent(new CustomEvent('cloud-job', { bubbles: true, detail: res.job }));
    toast(`${res.job?.label ?? '작업'}을 시작했습니다`, 'info');
  } catch (err) {
    toastError(err);
  } finally {
    btn.disabled = false;
  }
}

function jobCard(st) {
  const job = st.job;
  if (job) {
    const done = job.done ? ` · ${job.done.toLocaleString()}` : '';
    return h('div.card.cloud-job', {},
      h('div.card-title', {}, h('span', {}, job.label), badge('진행 중', 'info')),
      h('p', {}, `${job.stage}${done}`),
      h('p.muted.small', {}, `${relativeTime(job.startedAt)} 시작 · 이 화면을 닫아도 작업은 계속됩니다`));
  }
  const last = st.lastJob;
  if (!last) return null;
  return h('div.card.cloud-job', {},
    h('div.card-title', {},
      h('span', {}, `마지막 작업 — ${last.label}`),
      last.error ? badge('실패', 'danger') : badge('완료', 'success')),
    h('p', { class: last.error ? 'text-danger' : '' }, last.error || last.result || ''),
    h('p.muted.small', {}, `${formatDate(last.finishedAt ?? last.startedAt)}`));
}

// ---------- D1 ----------

function d1Card(data, statusBox, reload) {
  const cfg = data.d1 ?? {};
  const limits = data.limits ?? {};
  const enabledBox = checkbox('D1 동기화 켜기', { checked: Boolean(cfg.enabled) });
  const accountInput = input({ value: cfg.accountId ?? '', placeholder: '32자리 16진 (대시보드 오른쪽 아래)' });
  const dbInput = input({ value: cfg.databaseId ?? '', placeholder: 'xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx' });
  const tokenInput = input({ type: 'password', value: '', placeholder: cfg.apiToken || 'D1 편집 권한이 있는 API 토큰', autocomplete: 'off' });
  const intervalInput = input({
    type: 'number', min: limits.intervalMin ?? 10, max: limits.intervalMax ?? 3600,
    value: cfg.intervalSec ?? 30,
  });

  const save = h('button.btn.btn-primary', { type: 'button' }, icon('save'), '저장');
  const test = h('button.btn', { type: 'button' }, icon('link'), '연결 확인');
  const syncNow = h('button.btn', { type: 'button' }, icon('refresh'), '지금 동기화');
  const remoteBox = h('div');

  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      const body = {
        enabled: enabledBox.querySelector('input').checked,
        accountId: accountInput.value,
        databaseId: dbInput.value,
        intervalSec: Number(intervalInput.value) || 30,
      };
      // 빈 칸은 "바꾸지 않음"이다. 보내면 저장된 토큰이 지워진다.
      if (tokenInput.value.trim()) body.apiToken = tokenInput.value.trim();
      await api.put('/cloud/d1', body);
      toast('D1 설정을 저장했습니다', 'success');
      reload();
    } catch (err) {
      toastError(err);
      save.disabled = false;
    }
  });

  test.addEventListener('click', async () => {
    test.disabled = true;
    mount(remoteBox, spinner('D1 에 묻는 중…'));
    try {
      const res = await api.post('/cloud/d1/test');
      mount(remoteBox, remoteCard(res.remote));
    } catch (err) {
      mount(remoteBox, h('p.notice.notice-danger', {}, icon('alert'), err.message));
    } finally {
      test.disabled = false;
    }
  });

  syncNow.addEventListener('click', async () => {
    try {
      await api.post('/cloud/d1/sync');
      toast('곧 동기화합니다', 'info');
    } catch (err) {
      toastError(err);
    }
  });

  const pull = h('button.btn.btn-danger', { type: 'button' }, icon('history'), 'D1 의 데이터 가져오기');
  pull.addEventListener('click', async () => {
    const ok = await confirmDialog({
      title: 'D1 의 데이터 가져오기',
      message: '이 서버의 데이터(사용자·커넥션·ERD·감사 기록 전부)를 D1 의 내용으로 덮어씁니다. '
        + '덮어쓰기 전의 상태는 data/cloud/safety 에 사본으로 남습니다. 끝나면 다시 로그인해야 합니다 — '
        + 'D1 에 있던 계정으로 로그인하세요. 가져온 뒤부터는 이 서버가 D1 의 주인이 되고, 원래 서버는 더 이상 올리지 않습니다.',
      confirmLabel: '가져오기',
      danger: true,
      requireText: '가져오기',
    });
    if (ok) startJob(pull, '/cloud/d1/pull');
  });
  const push = h('button.btn', { type: 'button' }, icon('share'), '이 서버의 데이터로 D1 덮어쓰기');
  push.addEventListener('click', async () => {
    const ok = await confirmDialog({
      title: 'D1 덮어쓰기',
      message: 'D1 에 있는 내용을 지우고 이 서버의 데이터를 통째로 올립니다. 다른 환경이 그 D1 을 쓰고 있었다면 '
        + '그 환경은 이후 올리기를 멈춥니다.',
      confirmLabel: '덮어쓰기',
      danger: true,
      requireText: '덮어쓰기',
    });
    if (ok) startJob(push, '/cloud/d1/push');
  });

  return h('div.card', {},
    h('div.card-title', {}, h('span', {}, 'Cloudflare D1 동기화'), statusBadge(data.status?.d1?.state)),
    h('p.field-help', {},
      '로컬 SQLite 는 그대로 원본입니다. 화면은 계속 로컬에 읽고 쓰고, 바뀐 행은 브라우저가 아니라 서버가 모아 두었다가 ',
      '주기마다 한 번에 D1 에 반영합니다(같은 행을 여러 번 고치면 마지막 상태 하나만 갑니다). ',
      '지표·세션·클러스터 노드 목록처럼 이 서버에서만 뜻이 있는 것은 보내지 않습니다.'),
    enabledBox,
    h('div.form-grid', {},
      field('계정 ID', accountInput),
      field('데이터베이스 ID', dbInput),
    ),
    field('API 토큰', tokenInput, cfg.apiToken
      ? `지금 저장된 값: ${cfg.apiToken} (바꿀 때만 입력하세요). 암호화해 저장합니다.`
      : 'Cloudflare 대시보드 → 내 프로필 → API 토큰에서 "D1 편집" 권한으로 만드세요. 암호화해 저장합니다.'),
    field('동기화 주기(초)', intervalInput,
      '짧을수록 서버가 갑자기 꺼졌을 때 잃는 양이 적습니다. Cloudflare API 한도(5분에 1,200번)를 생각해 10초 이상으로 둡니다.'),
    h('div.notify-actions', {}, save, test, syncNow),
    statusBox,
    remoteBox,
    h('details.cloud-danger', {},
      h('summary', {}, '가져오기 · 덮어쓰기 (새 환경으로 옮길 때)'),
      h('p.field-help', {},
        '새 서버에서 같은 D1 을 연결했다면 [가져오기]로 원래 서버의 데이터를 받아 그대로 운영합니다. ',
        h('b', {}, '원래 서버와 같은 마스터 키'),
        '(DBSTUDIO_MASTER_KEY 또는 data/master.key)로 띄워야 합니다 — 키가 다르면 저장된 DB 자격증명을 풀 수 없어 가져오기를 거절합니다.'),
      h('div.notify-actions', {}, pull, push)),
  );
}

function statusBadge(state) {
  const [label, kind] = D1_STATE[state] ?? D1_STATE.off;
  return badge(label, kind);
}

function d1Status(st, cfg) {
  if (st.state === 'replica') return null;
  if (!cfg.enabled) {
    return h('p.muted.small', {}, '동기화가 꺼져 있습니다. 켜면 변경 추적을 시작하고, D1 이 비어 있으면 곧바로 전체를 올립니다.');
  }
  const rows = [
    ['마지막 동기화', st.lastSyncAt ? `${relativeTime(st.lastSyncAt)} (보낸 행 ${st.lastSent ?? 0}개)` : '아직 없음'],
    ['보낼 변경', st.pending ? `${st.pending}건 대기` : '없음'],
  ];
  if (st.nextSyncAt) rows.push(['다음 동기화', untilTime(st.nextSyncAt)]);
  return h('div.cloud-status', {},
    st.message ? h('p.notice.notice-warn', {}, icon('alert'), st.message) : null,
    st.lastError
      ? h('p.notice.notice-danger', {}, icon('alert'),
        `${st.lastError} (${relativeTime(st.errorAt)}) — 다음 주기에 다시 시도합니다`)
      : null,
    h('dl.cloud-facts', {}, rows.flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, v)])),
    st.remote ? remoteCard(st.remote) : null,
  );
}

function remoteCard(r) {
  if (!r) return null;
  if (!r.exists) {
    return h('p.notice', {}, icon('check'),
      '연결됐습니다. D1 은 비어 있습니다 — 동기화를 켜면 이 서버의 데이터를 올리고 이 서버가 주인이 됩니다.');
  }
  const facts = [
    ['주인', r.mine ? `이 서버 (${r.ownerName || '-'})` : (r.ownerName || r.owner || '없음')],
    ['행', `${r.rows.toLocaleString()}개`],
    ['상태', r.state === 'ready' ? '완료' : r.state === 'pushing' ? '올리는 도중 멈춤' : (r.state || '-')],
    ['마지막으로 올린 때', r.pushedAt ? formatDate(r.pushedAt) : '-'],
    ['스키마', `${r.schemaVersion} (이 서버 ${r.localSchema})`],
    ['마스터 키', r.keyMatches ? '이 서버와 같음' : '다름 — 가져올 수 없습니다'],
  ];
  return h('div.cloud-remote', {},
    h('p.field-help', {}, `D1 에 있는 것 (${relativeTime(r.checkedAt)} 확인)`),
    h('dl.cloud-facts', {}, facts.flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, v)])),
    !r.mine && r.rows > 0
      ? h('p.notice.notice-warn', {}, icon('alert'),
        '다른 환경의 데이터입니다. 아래 [가져오기]로 받아 이어서 쓰거나, [덮어쓰기]로 이 서버의 데이터를 올리세요. '
        + '고르기 전에는 D1 에 아무것도 쓰지 않습니다.')
      : null);
}

// ---------- 오브젝트 스토리지 ----------

function objectCard(data, statusBox, reload) {
  const cfg = data.object ?? {};
  const providerSelect = select(PROVIDERS, { value: cfg.provider || 'r2' });
  const accountInput = input({ value: cfg.accountId ?? '', placeholder: 'Cloudflare 계정 ID' });
  const regionInput = input({ value: cfg.region ?? '', placeholder: '예: ap-northeast-2' });
  const endpointInput = input({ value: cfg.endpoint ?? '', placeholder: 'https://minio.example.com:9000' });
  const bucketInput = input({ value: cfg.bucket ?? '', placeholder: 'dbstudio-backups' });
  const prefixInput = input({ value: cfg.prefix ?? 'dbstudio/', placeholder: 'dbstudio/' });
  const accessInput = input({ value: cfg.accessKey ?? '', autocomplete: 'off' });
  const secretInput = input({ type: 'password', value: '', placeholder: cfg.secretKey || '시크릿 액세스 키', autocomplete: 'off' });
  const virtualBox = checkbox('가상 호스트 방식 주소(bucket.host)', { checked: Boolean(cfg.virtualHost) });
  const encryptBox = checkbox('마스터 키로 암호화해 올리기 (권장)', { checked: cfg.encrypt !== false });
  const scheduleBox = checkbox('정기 백업', { checked: Boolean(cfg.scheduleEnabled) });
  const everySelect = select(EVERY, { value: String(cfg.everyHours ?? 24) });
  const atInput = input({ type: 'time', value: cfg.at || '03:00' });
  const keepInput = input({ type: 'number', min: 0, max: 1000, value: cfg.keep ?? 14 });

  const accountField = field('R2 계정 ID', accountInput,
    '엔드포인트는 https://<계정 ID>.r2.cloudflarestorage.com 으로 정해집니다. R2 API 토큰의 액세스 키·시크릿 키를 씁니다.');
  const regionField = field('리전', regionInput);
  const endpointField = field('엔드포인트', endpointInput);
  const syncProvider = () => {
    const p = providerSelect.value;
    accountField.style.display = p === 'r2' ? '' : 'none';
    regionField.style.display = p === 'r2' ? 'none' : '';
    endpointField.style.display = p === 'r2' ? 'none' : '';
    virtualBox.style.display = p === 'r2' ? 'none' : '';
    endpointInput.placeholder = p === 's3'
      ? '비우면 https://s3.<리전>.amazonaws.com'
      : 'https://minio.example.com:9000';
  };
  providerSelect.addEventListener('change', syncProvider);
  syncProvider();

  const save = h('button.btn.btn-primary', { type: 'button' }, icon('save'), '저장');
  const test = h('button.btn', { type: 'button' }, icon('link'), '연결 확인');
  const backupNow = h('button.btn', { type: 'button' }, icon('play'), '지금 백업');

  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      const body = {
        provider: providerSelect.value,
        accountId: accountInput.value,
        region: regionInput.value,
        endpoint: endpointInput.value,
        bucket: bucketInput.value,
        prefix: prefixInput.value,
        accessKey: accessInput.value,
        virtualHost: virtualBox.querySelector('input').checked,
        encrypt: encryptBox.querySelector('input').checked,
        scheduleEnabled: scheduleBox.querySelector('input').checked,
        everyHours: Number(everySelect.value),
        at: atInput.value || '03:00',
        keep: Number(keepInput.value) || 0,
      };
      if (secretInput.value.trim()) body.secretKey = secretInput.value.trim();
      await api.put('/cloud/object', body);
      toast('백업 설정을 저장했습니다', 'success');
      reload();
    } catch (err) {
      toastError(err);
      save.disabled = false;
    }
  });
  test.addEventListener('click', async () => {
    test.disabled = true;
    try {
      const res = await api.post('/cloud/object/test');
      toast(`연결됐습니다 — ${res.location} (백업 ${res.backups}개)`, 'success', 6000);
    } catch (err) {
      toastError(err);
    } finally {
      test.disabled = false;
    }
  });
  backupNow.addEventListener('click', () => startJob(backupNow, '/cloud/object/backups'));

  return h('div.card', {},
    h('div.card-title', {}, h('span', {}, '오브젝트 스토리지 백업 (R2 · S3)'),
      cfg.scheduleEnabled ? badge('정기 백업 켜짐', 'success') : badge('수동', 'neutral')),
    h('p.field-help', {},
      '메타 DB 를 일관된 시점으로 떠서(VACUUM INTO) gzip 으로 묶어 올립니다. 마스터 키는 올리지 않습니다 — ',
      '복구하려면 원래 서버와 같은 키로 띄운 DB Studio 가 필요합니다. 대상 DB 의 덤프(백업 화면)와는 다른 것입니다.'),
    field('종류', providerSelect),
    accountField,
    h('div.form-grid', {}, regionField, endpointField),
    virtualBox,
    h('div.form-grid', {},
      field('버킷', bucketInput),
      field('경로 접두사', prefixInput, '버킷 안에서 백업을 둘 자리입니다. 한 버킷을 여러 설치본이 나눠 쓸 수 있습니다.'),
    ),
    h('div.form-grid', {},
      field('액세스 키', accessInput),
      field('시크릿 키', secretInput, cfg.secretKey
        ? `지금 저장된 값: ${cfg.secretKey} (바꿀 때만 입력하세요)`
        : '암호화해 저장합니다'),
    ),
    encryptBox,
    h('div.cloud-schedule', {},
      scheduleBox,
      h('div.form-grid', {},
        field('주기', everySelect),
        field('기준 시각 (서버 시각)', atInput, '이 시각을 기준으로 주기마다 돕니다. 서버가 꺼져 있어 놓치면 켜질 때 한 번 따라잡습니다.'),
        field('남길 개수', keepInput, '넘으면 오래된 백업부터 지웁니다. 0이면 지우지 않습니다. 이 앱이 만든 백업만 지웁니다.'),
      )),
    h('div.notify-actions', {}, save, test, backupNow),
    statusBox,
  );
}

function objectStatus(st) {
  const rows = [
    ['마지막 백업', st.lastBackupAt ? `${formatDate(st.lastBackupAt)} (${relativeTime(st.lastBackupAt)})` : '아직 없음'],
  ];
  if (st.nextBackupAt) rows.push(['다음 정기 백업', `${formatDate(st.nextBackupAt)} (${untilTime(st.nextBackupAt)})`]);
  return h('div.cloud-status', {},
    st.lastError
      ? h('p.notice.notice-danger', {}, icon('alert'), `${st.lastError} (${relativeTime(st.errorAt)})`)
      : null,
    h('dl.cloud-facts', {}, rows.flatMap(([k, v]) => [h('dt', {}, k), h('dd', {}, v)])));
}

async function loadBackups(box) {
  mount(box, h('div.card', {}, h('h2.card-title', {}, '버킷의 백업'), spinner('목록을 읽는 중…')));
  let list;
  try {
    list = (await api.get('/cloud/object/backups')).backups ?? [];
  } catch (err) {
    mount(box, h('div.card', {}, h('h2.card-title', {}, '버킷의 백업'),
      h('p.notice.notice-danger', {}, icon('alert'), err.message)));
    return;
  }
  const refresh = h('button.btn.btn-sm', { type: 'button', onclick: () => loadBackups(box) }, icon('refresh'), '새로고침');
  if (list.length === 0) {
    mount(box, h('div.card', {}, h('div.card-title', {}, h('span', {}, '버킷의 백업'), refresh),
      h('p.muted', {}, '아직 올린 백업이 없습니다. [지금 백업]으로 하나 만들어 보세요.')));
    return;
  }
  mount(box, h('div.card', {},
    h('div.card-title', {}, h('span', {}, `버킷의 백업 (${list.length})`), refresh),
    h('table.table', {},
      h('thead', {}, h('tr', {}, h('th', {}, '이름'), h('th', {}, '크기'), h('th', {}, '올린 때'), h('th', {}, ''))),
      h('tbody', {}, list.map((b) => {
        const restore = h('button.btn.btn-sm.btn-danger', { type: 'button' }, icon('history'), '이 백업으로 복구');
        restore.addEventListener('click', async () => {
          const ok = await confirmDialog({
            title: '백업으로 복구',
            message: `이 서버의 데이터를 ${b.name} 시점으로 되돌립니다. 그 뒤에 한 일(새 사용자·ERD 편집·감사 기록)은 사라집니다. `
              + '되돌리기 전의 상태는 data/cloud/safety 에 사본으로 남습니다. 지금 로그인한 세션은 유지되지만, '
              + '백업에 없는 계정이면 다시 로그인해야 합니다. D1 동기화가 켜져 있으면 D1 도 이 시점으로 다시 올립니다.',
            confirmLabel: '복구',
            danger: true,
            requireText: '복구',
          });
          if (ok) startJob(restore, '/cloud/object/restore', { key: b.key });
        });
        return h('tr', {},
          h('td', {}, h('code', {}, b.name), b.encrypted ? ' ' : null, b.encrypted ? badge('암호화', 'info') : null),
          h('td', {}, humanSize(b.size)),
          h('td', {}, `${formatDate(b.modifiedAt)}`),
          h('td.cloud-actions', {}, restore));
      })))));
}

// untilTime은 앞으로 올 시각을 "○분 뒤"로 읽는다. relativeTime은 지난 시각만 다룬다
// (앞으로 올 시각을 넣으면 "방금"이 된다). 이미 지났으면 곧 한다는 뜻이다.
function untilTime(value) {
  const min = Math.round((new Date(value).getTime() - Date.now()) / 60000);
  if (Number.isNaN(min)) return '—';
  if (min <= 0) return '곧';
  if (min < 60) return `${min}분 뒤`;
  const hour = Math.floor(min / 60);
  if (hour < 48) return `약 ${hour}시간 뒤`;
  return `약 ${Math.floor(hour / 24)}일 뒤`;
}

function humanSize(n) {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`;
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`;
  if (n >= 1 << 10) return `${(n / (1 << 10)).toFixed(1)} KB`;
  return `${n} B`;
}

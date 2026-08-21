// 2단계 인증(TOTP) 화면 조각.
//
// 등록 절차를 한 곳에 모아 둔 이유: 같은 절차가 두 자리에서 쓰인다.
//   - 프로필 화면에서 자발적으로 켤 때(모달)
//   - 관리자가 의무화해서 등록 전에는 아무것도 못 할 때(전용 화면)
// 두 곳의 문구나 단계가 갈라지면 "어느 쪽이 맞는 절차인가"를 아무도 모르게 된다.
import { api } from '../core/api.js';
import { loadSession } from '../core/store.js';
import {
  h, mount, icon, field, input, spinner, toast, toastError,
  openModal, copyToClipboard,
} from '../core/ui.js';

// codeInput은 인증 코드 입력칸이다.
//
// inputmode/autocomplete를 지정하는 이유: 휴대폰에서는 숫자 자판이 바로 떠야 하고,
// iOS·안드로이드는 one-time-code 힌트를 보고 알림에 뜬 코드를 자동 채워 준다.
// 6자리를 손으로 옮겨 적는 일을 없애 주는 것이 이 기능의 체감 품질을 좌우한다.
export function codeInput(props = {}) {
  return input({
    inputmode: 'numeric',
    autocomplete: 'one-time-code',
    autocorrect: 'off',
    spellcheck: false,
    maxlength: 20,
    placeholder: '000000',
    class: 'code-input',
    ...props,
  });
}

// enrollPanel은 등록 절차(QR → 코드 확인 → 복구 코드)를 담은 노드를 만든다.
// onDone은 복구 코드까지 확인하고 끝냈을 때 불린다.
export function enrollPanel({ onDone, onCancel }) {
  const box = h('div.totp-enroll');

  const start = async () => {
    mount(box, spinner('등록 정보를 준비하는 중…'));
    let enrollment;
    try {
      ({ enrollment } = await api.post('/auth/totp/setup'));
    } catch (err) {
      mount(box,
        h('p.notice.notice-danger', {}, icon('alert'), err.message),
        h('button.btn', { type: 'button', onclick: start }, icon('refresh'), '다시 시도'),
      );
      return;
    }
    mount(box, stepScan(enrollment, onDone, onCancel));
  };

  start();
  return box;
}

function stepScan(enrollment, onDone, onCancel) {
  const code = codeInput();
  const submit = h('button.btn.btn-primary', { type: 'submit' }, '확인하고 켜기');
  const notice = h('div');

  const form = h('form.totp-form', {
    onsubmit: async (e) => {
      e.preventDefault();
      if (!code.value.trim()) {
        toast('인증 앱에 표시된 6자리 코드를 입력하세요', 'error');
        code.focus();
        return;
      }
      submit.disabled = true;
      try {
        const res = await api.post('/auth/totp/confirm', { code: code.value.trim() });
        await loadSession();
        mount(notice);
        showRecoveryCodes(res.recoveryCodes, onDone);
      } catch (err) {
        code.value = '';
        code.focus();
        // 등록 단계의 실패는 대부분 "코드를 늦게 넣었다"이므로,
        // 오류 문구보다 다음에 할 일을 크게 보여 준다.
        mount(notice, h('p.notice.notice-warn', {}, icon('alert'),
          `${err.message} 인증 앱에 지금 떠 있는 코드로 다시 시도하세요.`));
      } finally {
        submit.disabled = false;
      }
    },
  },
    field('인증 코드', code, '인증 앱에 표시된 6자리 숫자를 입력하세요'),
    notice,
    h('div.form-actions', {},
      onCancel ? h('button.btn', { type: 'button', onclick: onCancel }, '취소') : null,
      submit,
    ),
  );

  return h('div', {},
    h('ol.totp-steps', {},
      h('li', {}, 'Google Authenticator, Microsoft Authenticator, 1Password 같은 인증 앱을 준비합니다.'),
      h('li', {}, '앱에서 “계정 추가”를 누르고 아래 QR 코드를 찍습니다.'),
      h('li', {}, '앱에 표시된 6자리 코드를 입력해 등록을 마칩니다.'),
    ),
    h('div.totp-scan', {},
      enrollment.qr
        ? h('img.totp-qr', { src: enrollment.qr, alt: 'QR 코드', width: 200, height: 200 })
        : h('p.notice.notice-warn', {}, icon('alert'),
          'QR 코드를 만들지 못했습니다. 아래 키를 직접 입력하세요.'),
      h('div.totp-manual', {},
        h('div.field-label', {}, 'QR을 못 찍을 때 쓰는 키'),
        h('code.totp-secret', {}, enrollment.formattedSecret),
        h('button.btn.btn-small', {
          type: 'button', onclick: () => copyToClipboard(enrollment.secret),
        }, icon('copy'), '키 복사'),
        h('p.field-help', {},
          `시간 기반(TOTP) · ${enrollment.digits}자리 · ${enrollment.period}초 · SHA-1`),
      ),
    ),
    form,
  );
}

// showRecoveryCodes는 복구 코드를 한 번만 보여준다.
// 서버는 해시만 갖고 있으므로 이 창을 닫으면 되찾을 방법이 없다.
export function showRecoveryCodes(codes, onDone) {
  const text = codes.join('\n');
  openModal({
    title: '복구 코드',
    width: 560,
    body: () => [
      h('p.notice.notice-warn', {}, icon('alert'),
        h('span', {},
          h('b', {}, '지금 저장하세요. 이 화면을 닫으면 다시 볼 수 없습니다. '),
          '휴대폰을 잃어버렸을 때 로그인할 수 있는 유일한 수단입니다. 각 코드는 한 번만 쓸 수 있습니다.')),
      h('div.recovery-codes', {}, codes.map((c) => h('code', {}, c))),
      h('div.form-actions', {},
        h('button.btn', { type: 'button', onclick: () => copyToClipboard(text) },
          icon('copy'), '모두 복사'),
        h('button.btn', { type: 'button', onclick: () => downloadCodes(text) },
          icon('save'), '텍스트로 저장'),
      ),
      h('p.field-help', {},
        '비밀번호 관리자나 인쇄물처럼 휴대폰과 다른 곳에 보관하세요. ' +
        '둘을 같은 기기에 두면 그 기기를 잃는 순간 두 수단을 함께 잃습니다.'),
    ],
    footer: (close) => [
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: () => { close(); onDone?.(); },
      }, '저장했습니다'),
    ],
  });
}

function downloadCodes(text) {
  const blob = new Blob([`DB Studio 2단계 인증 복구 코드\n\n${text}\n`], {
    type: 'text/plain;charset=utf-8',
  });
  const url = URL.createObjectURL(blob);
  const a = h('a', { href: url, download: 'dbstudio-recovery-codes.txt' });
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// openTOTPSetup은 프로필 화면에서 쓰는 모달 버전이다.
export function openTOTPSetup(onDone) {
  let close = null;
  const panel = enrollPanel({
    onDone: () => { close?.(); onDone?.(); },
    onCancel: () => close?.(),
  });
  close = openModal({
    title: '2단계 인증 설정',
    width: 620,
    body: () => panel,
  });
}

// renderTOTPSetupPage는 의무화 상태에서 등록을 마칠 때까지 다른 화면 진입을 막는 전용 화면이다.
// (비밀번호 강제 변경 화면과 같은 자리, 같은 모양을 쓴다.)
export function renderTOTPSetupPage(outlet, { onSuccess, username }) {
  mount(outlet,
    h('div.auth-shell', {},
      h('div.auth-card.auth-card-wide', {},
        h('div.auth-brand', {}, icon('shield', 26), h('h1', {}, '2단계 인증 설정 필요')),
        h('p.auth-sub', {},
          `${username ?? ''} 계정에 2단계 인증을 설정해야 계속할 수 있습니다. ` +
          '관리자가 모든 사용자에게 2단계 인증을 의무화했습니다.'),
        enrollPanel({ onDone: () => onSuccess?.() }),
      ),
    ),
  );
}

// disableDialog는 해제 확인 창이다. 비밀번호를 다시 묻는다.
export function openTOTPDisable(onDone) {
  const password = input({ type: 'password', autocomplete: 'current-password' });
  openModal({
    title: '2단계 인증 해제',
    width: 480,
    body: () => [
      h('p.notice.notice-warn', {}, icon('alert'),
        '해제하면 비밀번호만으로 로그인할 수 있게 되고, 복구 코드도 함께 폐기됩니다.'),
      field('현재 비밀번호', password, '본인 확인을 위해 다시 입력하세요'),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-danger', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            await api.post('/auth/totp/disable', { password: password.value });
            await loadSession();
            toast('2단계 인증을 해제했습니다', 'success');
            close();
            onDone?.();
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, '해제'),
    ],
  });
}

// openRecoveryRegenerate는 복구 코드를 다시 발급한다.
// 지금 인증 앱을 들고 있는 사람만 할 수 있어야 하므로 코드를 묻는다.
export function openRecoveryRegenerate(onDone) {
  const code = codeInput();
  openModal({
    title: '복구 코드 재발급',
    width: 480,
    body: () => [
      h('p.field-help', {},
        '새 코드를 발급하면 기존 복구 코드는 모두 무효가 됩니다. ' +
        '본인 확인을 위해 인증 앱의 코드를 입력하세요.'),
      field('인증 코드', code),
    ],
    footer: (close) => [
      h('button.btn', { type: 'button', onclick: close }, '취소'),
      h('button.btn.btn-primary', {
        type: 'button',
        onclick: async (e) => {
          e.currentTarget.disabled = true;
          try {
            const res = await api.post('/auth/totp/recovery', { code: code.value.trim() });
            close();
            showRecoveryCodes(res.recoveryCodes, onDone);
          } catch (err) {
            toastError(err);
            e.currentTarget.disabled = false;
          }
        },
      }, '재발급'),
    ],
  });
}

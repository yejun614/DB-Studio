// 설계 화면에서 쓰는 색.
//
// ERD 편집기와 구조 화면이 **같은 목록**을 써야 두 화면이 같은 언어로 읽힌다. 전에는
// 두 파일이 각자 목록을 들고 있었고, 그 사실을 아는 주석까지 있었다("같은 팔레트여야
// 한다") — 주석으로 지킬 수 있는 약속이 아니다.
//
// 값은 CSS 변수가 아니라 **실제 색**이다. 문서(Box.color·Group.color·Note.color)에
// 그대로 담겨 서버로 가고, 그림으로 내보낼 때도 그 값이 쓰인다. 테마에 따라 달라지는
// 값을 담으면 어두운 화면에서 칠한 색이 밝은 화면에서 다른 색이 된다.
import { h } from './dom.js';

// 색상환 차례로 둔다. 고르는 사람은 "노랑 옆의 조금 더 붉은 것"처럼 찾는다.
//
// 기존 다섯(노랑·파랑·초록·분홍·회색)의 값은 그대로 둔다. 값이 곧 저장된 데이터라,
// 조금이라도 바꾸면 이미 칠해 둔 카드가 "고른 적 없는 색"이 되어 고르개에서 아무
// 스와치에도 불이 들어오지 않는다.
export const TINTS = [
  { value: '#ef4444', label: '빨강' },
  { value: '#f97316', label: '주황' },
  { value: '#eab308', label: '노랑' },
  { value: '#84cc16', label: '연두' },
  { value: '#22c55e', label: '초록' },
  { value: '#14b8a6', label: '청록' },
  { value: '#0ea5e9', label: '하늘' },
  { value: '#3b82f6', label: '파랑' },
  { value: '#6366f1', label: '남색' },
  { value: '#a855f7', label: '보라' },
  { value: '#d946ef', label: '자주' },
  { value: '#ec4899', label: '분홍' },
  { value: '#b45309', label: '갈색' },
  { value: '#a1a1aa', label: '회색' },
];

// 기본(색 없음)을 앞에 둔 목록. 카드처럼 "칠하지 않음"이 뜻을 갖는 곳에서 쓴다.
export const TINT_CHOICES = [{ value: '', label: '기본' }, ...TINTS];

/**
 * tintPicker는 색 고르개를 만든다.
 *
 * 노드와 함께 set()을 돌려주는 이유: 고른 자리를 옮기는 방법이 화면마다 다르다.
 * 인스펙터는 문서가 바뀌면 통째로 다시 그리지만, 만들기 창은 아직 아무것도 저장되지
 * 않은 상태에서 표시만 옮겨야 한다. 두 경우가 각자 DOM을 뒤지게 두면 같은 코드가
 * 세 벌 생긴다(실제로 그랬다).
 *
 * @param {object} opts
 * @param {string|null} opts.current 지금 색. null이면 아무것도 켜지 않는다 —
 *   여럿을 골랐는데 색이 서로 다를 때 "지금 모두 기본색"이라고 거짓말하지 않기 위해서다.
 * @param {(value: string) => void} opts.onPick
 * @param {boolean} [opts.withDefault] '기본'(색 없음)을 포함할지
 * @param {boolean} [opts.disabled] 읽기 전용
 */
export function tintPicker({ current, onPick, withDefault = true, disabled = false }) {
  const list = withDefault ? TINT_CHOICES : TINTS;
  const buttons = list.map((c) => h('button.tint-swatch', {
    type: 'button',
    // 기본만 클래스로 칠한다. 나머지는 값 자체가 색이라, 색마다 CSS 규칙을 두면
    // 색을 하나 더할 때마다 두 파일을 고쳐야 하고 언젠가 한쪽만 고쳐진다.
    class: c.value ? '' : 'tint-none',
    style: c.value ? { background: c.value } : null,
    title: c.label,
    'aria-label': c.label,
    disabled,
    onclick: () => onPick(c.value),
  }));

  const node = h('div.tint-picker', {}, buttons);
  const set = (value) => {
    list.forEach((c, i) => {
      buttons[i].classList.toggle('is-on', value != null && (value || '') === c.value);
    });
  };
  set(current);
  return { node, set };
}

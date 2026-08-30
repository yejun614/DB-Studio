// 미리 검사: 만들기 전에·실행하기 전에 SQL이 실제로 도는지 확인한다.
//
// 서버가 그림자 DB를 만들어 거기서 돌려 보고 지운다(대상 DB는 손대지 않는다).
// 화면이 하는 일은 그 결과를 세 갈래로 갈라 말하는 것뿐이다: 통과 / 막힘 /
// 검사하지 못함. 마지막을 실패로 뭉뚱그리면 권한이 없어 못 해 본 것을 계획이 틀린
// 것으로 읽게 되고, 사람은 멀쩡한 초안을 뜯어고치기 시작한다.
//
// ERD 설계와 마이그레이션 화면이 같은 것을 쓴다. 두 곳에 따로 그리면 한쪽 문구만
// 고쳐져서, 같은 검사가 화면마다 다른 말을 하게 된다.
import { api } from './api.js';
import { h, mount, icon, spinner } from './ui.js';
import { codeBlock } from './highlight.js';

// runDryRun은 검사를 요청하고 결과를 box에 그린다. 통과했으면 true다.
export async function runDryRun({ path, body, box, button, label = '그림자 DB에서 실행해 보는 중…' }) {
  const was = button?.disabled ?? false;
  if (button) button.disabled = true;
  mount(box, spinner(label));
  try {
    const res = await api.post(path, body ?? {});
    mount(box, dryRunView(res));
    return res.dryRun?.ok === true && !res.dryRun?.skipped;
  } catch (err) {
    mount(box, h('div.notice.notice-danger', {}, icon('alert'),
      h('div', {},
        h('strong', {}, '미리 검사를 하지 못했습니다'),
        h('p', {}, err.message ?? String(err)),
        err.detail ? h('p.muted', {}, err.detail) : null)));
    return false;
  } finally {
    if (button) button.disabled = was;
  }
}

// dryRunView는 미리 검사 결과를 그린다.
export function dryRunView(res) {
  const r = res.dryRun ?? {};
  if (r.skipped) {
    return h('div.notice.notice-warn', {}, icon('alert'),
      h('div', {},
        h('strong', {}, '미리 실행해 보지 못했습니다'),
        h('p', {}, r.skipped),
        r.seedFailed
          ? h('p.muted', {},
            '기준 구조를 그림자 DB에 세우는 단계에서 막혔습니다. '
            + '계획 자체는 아직 시험해 보지 못했습니다.')
          : null));
  }
  if (r.ok) {
    return h('div.notice.notice-success', {}, icon('check'),
      h('div', {},
        h('strong', {}, `${res.statements ?? 0}문장이 모두 실행됐습니다`),
        h('p.muted', {},
          `${res.base ?? '기준'} 위에 그림자 DB(${r.where ?? '임시'})를 만들어 돌려 봤고, `
          + '검사 뒤 지웠습니다. 대상 DB는 그대로입니다.')));
  }
  const failed = (r.steps ?? []).find((s) => s.error);
  return h('div', {},
    h('div.notice.notice-danger', {}, icon('alert'),
      h('div', {},
        h('strong', {}, `${(r.failedIndex ?? 0) + 1}번째 문장에서 막혔습니다`),
        h('p', {}, r.error ?? ''),
        h('p.muted', {}, res.after ?? '초안을 고친 뒤 다시 검사하세요.'))),
    // 막힌 문장을 그대로 보여준다. "1번째 문장에서 막혔습니다"만으로는 어느
    // 테이블의 어느 컬럼이 문제인지 알 수 없다.
    failed
      ? h('div.dryrun-stmt', {},
        h('span.field-label', {}, '막힌 문장'),
        codeBlock(failed.sql, 'sql', { className: 'sql-block' }))
      : null,
    // 여기까지는 됐다는 사실도 필요하다. 어디까지 갔는지를 알면 원인이 좁혀진다.
    (r.failedIndex ?? 0) > 0
      ? h('p.muted', {}, `앞의 ${r.failedIndex}문장은 그림자 DB에서 문제없이 실행됐습니다.`)
      : null,
  );
}

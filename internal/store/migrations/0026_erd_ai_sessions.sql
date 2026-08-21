-- P19: ERD 초안에 매인 AI 대화.
--
-- 설계 화면의 "대화"는 지금까지 참여자들이 함께 쓰는 방 하나였다. 여기에 AI와의
-- 대화를 더하는데, 그 둘을 같은 곳에 섞지 않는다.
--
-- 섞지 않는 이유: 사람에게 하는 말과 모델에게 하는 말은 성격이 다르다. 모델과는
-- 시행착오를 반복하고("아니 그 컬럼 말고"), 그 과정 전부가 방에 흘러가면 정작
-- 사람끼리의 결정이 묻힌다. 그래서 AI 대화는 **개인의 것**이고, 그중 남길 만한
-- 답만 사용자가 골라 방으로 공유한다.
--
-- 새 표를 만들지 않고 ai_sessions에 열을 더한 이유: 이것은 여전히 AI 세션이다.
-- 프로바이더·모델·토큰 집계·메시지 저장이 모두 같고, 다른 것은 "무엇에 대한
-- 대화인가"뿐이다. 표를 나누면 그 공통 부분이 두 벌이 된다.
--
-- 문서가 지워지면 그 문서에 대한 대화도 함께 사라진다. 대상이 없는 설계 대화는
-- 읽어도 무슨 이야기인지 알 수 없다.
ALTER TABLE ai_sessions ADD COLUMN erd_document_id TEXT
    REFERENCES erd_documents (id) ON DELETE CASCADE;

CREATE INDEX idx_ai_sessions_erd ON ai_sessions (erd_document_id, user_id, updated_at DESC);

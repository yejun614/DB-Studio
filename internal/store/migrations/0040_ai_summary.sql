-- 대화 요약. 컨텍스트가 찼을 때 오래된 이야기를 한 문단으로 접어 둔다.
--
-- 지금까지는 예산을 넘으면 오래된 메시지를 **그냥 버렸다**. 최근 것이 더 유효하다는
-- 판단은 맞지만, 버린 사실이 아무 데도 적히지 않았다. 사람은 "아까 말한 그거"라고
-- 하는데 모델에게는 그 말이 없다.
--
-- 무엇을 요약하고 무엇을 버리는지는 api.compactHistory 에 적어 두었다. 요약이
-- 필요한 것은 사람이 한 이야기뿐이다 — 툴 결과는 요약하는 것보다 비우고 다시
-- 부르게 하는 편이 정확하다.
--
-- summary_through_id 를 함께 두는 이유: 요약이 어디까지를 담고 있는지 모르면
-- 같은 대목을 두 번 담거나(요약 + 원문) 빠뜨린다. 그리고 사람이 자기 말을 고쳐
-- 그 뒤를 지우면(replaceFrom) 그 자리를 담고 있던 요약은 **틀린 요약**이 된다.
-- 그때 이 값을 보고 버린다.
ALTER TABLE ai_sessions ADD COLUMN summary TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_sessions ADD COLUMN summary_through_id INTEGER NOT NULL DEFAULT 0;

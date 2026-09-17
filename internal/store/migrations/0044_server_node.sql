-- 담당 노드를 서버로 올린다.
--
-- ── 왜 옮기는가 ────────────────────────────────────────────────────
-- 0029는 담당 노드를 **커넥션(DB) 등급 사실**로 두었다("이 커넥션에 접속하는 노드다").
-- 그런데 "이 DB가 어느 사설망 안에 있는가"는 host·port·options·자격증명과 같은 급의
-- **접속 사실**이다. 그 넷은 전부 서버에 있는데 담당 노드만 DB에 있어서, 같은 사실을
-- 서버 아래 DB 개수만큼 반복 입력하게 되어 있었다. 하나만 빠뜨리면 그 DB만 조용히
-- 실패하고(증상은 "어떤 DB는 되고 어떤 DB는 안 된다"), 원인은 서버 등급 사실이라
-- 증상과 멀어진다.
--
-- ── 왜 예외를 남기는가 ─────────────────────────────────────────────
-- connections.node_id를 지우지 않는다. 그 칸은 이제 **서버를 덮는 예외**다. 우선순위는
-- DB 예외 → 서버 → 빈 값(요청을 받은 노드가 직접 접속)이고, 그 판정은 store가
-- COALESCE(NULLIF(c.node_id,''), s.node_id, '') 한 곳에서 한다. 그래야 기존에 DB마다
-- 지정해 둔 구성이 그대로 동작하고, 배선(라우팅·폴링·화면)을 새로 깔 필요가 없다.
ALTER TABLE servers ADD COLUMN node_id TEXT NOT NULL DEFAULT '';

-- 기존 데이터를 올린다.
--
-- 서버 아래 DB들이 같은 노드를 가리키면 그것이 곧 그 서버의 사실이다. 사람이 DB마다
-- 같은 값을 반복해 넣었다는 뜻이고, 그 값을 서버로 올리면 앞으로 새 DB는 자동으로
-- 따라간다.
--
-- 섞여 있으면 **가장 많이 지정된 노드**를 고른다. 그 경우는 예외 지정이 이미 섞여
-- 있다는 뜻이라 어느 쪽이 "서버의 사실"인지 데이터만으로는 알 수 없다. 다수결로
-- 올리고 나머지는 connections.node_id에 그대로 남겨 예외로 동작하게 한다 —
-- 여기서 아무것도 하지 않으면 앞으로 추가되는 DB가 담당 없이 생겨 더 나쁘다.
--
-- 갱신하지 않는 경우: 모든 DB가 담당 없음(빈 문자열). 서버도 빈 값으로 두는 것이 맞다.
UPDATE servers
SET node_id = (
    SELECT c.node_id
    FROM connections c
    WHERE c.server_id = servers.id AND c.node_id <> ''
    GROUP BY c.node_id
    ORDER BY COUNT(*) DESC, c.node_id
    LIMIT 1
)
WHERE EXISTS (
    SELECT 1 FROM connections c
    WHERE c.server_id = servers.id AND c.node_id <> ''
);

-- 노드가 담당인 서버를 찾는 질의가 생긴다(노드를 내릴 때 영향 범위를 세는 자리).
CREATE INDEX idx_servers_node ON servers (node_id);

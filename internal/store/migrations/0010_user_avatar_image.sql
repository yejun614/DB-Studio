-- P11: 업로드한 프로필 이미지.
--
-- 바이트를 메타 DB에 넣는다. 파일로 떨어뜨리면 단일 바이너리 원칙이 깨지고
-- (데이터가 바이너리 밖으로 새어 나간다) 백업·삭제·권한이 두 곳으로 갈라진다.
-- 이미지는 사용자당 한 장이고 상한이 있으므로(기본 512KB) DB에 두는 비용이 작다.
--
-- users.avatar가 'upload'일 때만 이 행이 쓰인다. 두 값을 한 테이블에 두지 않는 이유는
-- 아이콘을 골라 둔 사람의 행을 이미지 BLOB 열이 있는 테이블에서 읽고 싶지 않아서다 —
-- users는 로그인마다 읽힌다.
CREATE TABLE user_avatars (
    user_id    TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    mime       TEXT NOT NULL,            -- image/png | image/jpeg | image/gif | image/webp
    bytes      BLOB NOT NULL,
    size       INTEGER NOT NULL,
    width      INTEGER NOT NULL DEFAULT 0,
    height     INTEGER NOT NULL DEFAULT 0,
    -- source는 어디서 왔는지다: upload(파일 업로드) | uri(서버가 내려받음)
    source     TEXT NOT NULL DEFAULT 'upload',
    source_uri TEXT NOT NULL DEFAULT '',
    -- version은 이미지를 바꿀 때마다 올라간다. 이미지 URL에 붙여 캐시를 무효화한다.
    version    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL
);

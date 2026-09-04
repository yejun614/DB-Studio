package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbstudio/internal/erd"
)

// 문서 설정이 내보내는 SQL 에 실제로 나가는지 본다.
//
// 화면에서 고른 값이 스크립트에 안 나가는 것이 이 기능에서 가장 조용한 실패다 —
// 마이그레이션을 실행하고 나서야, 엔진이 서버 기본값인 표를 발견하게 된다.
func TestERDDocumentOptionsReachExportedSQL(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "상점", "mysql")

	ctx := context.Background()
	doc, err := e.st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("초안 읽기: %v", err)
	}
	no := 0
	apply := func(kind erd.Kind, payload string) {
		t.Helper()
		no++
		op := &erd.Op{ID: fmt.Sprintf("seed-%d", no), Kind: kind, Payload: []byte(payload)}
		if aerr := erd.Apply(doc, op); aerr != nil {
			t.Fatalf("op %s: %v", kind, aerr)
		}
		if serr := e.st.AppendERDOp(ctx, doc, op); serr != nil {
			t.Fatalf("op %s 저장: %v", kind, serr)
		}
	}

	apply(erd.OpDocOptions, `{"tableDefaults":{"engine":"InnoDB","charset":"utf8mb4"},`+
		`"targetDb":{"name":"shop","options":{"charset":"utf8mb4"}}}`)
	apply(erd.OpTableAdd, `{"name":"members","withId":true}`)
	apply(erd.OpTableAdd, `{"name":"orders","withId":true}`)
	// 한 표만 다르게 정한다.
	apply(erd.OpTableUpdate, `{"key":"orders","options":{"engine":"MyISAM"}}`)

	status, body := c.do("GET", "/api/v1/erd/documents/"+docID+"/ddl", nil)
	if status != 200 {
		t.Fatalf("ddl = %d: %v", status, body)
	}
	sql, _ := body["upSql"].(string)

	// 새 DB 는 맨 앞에서 만들어진다 — 그 다음 문장들이 그 안에서 돌아야 한다.
	lines := []string{}
	for _, line := range strings.Split(sql, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		t.Fatalf("문장이 너무 적습니다:\n%s", sql)
	}
	if !strings.HasPrefix(lines[0], "CREATE DATABASE") || !strings.Contains(lines[0], "shop") {
		t.Errorf("첫 문장이 CREATE DATABASE 가 아닙니다: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "USE") {
		t.Errorf("두 번째 문장이 USE 가 아닙니다 — 나머지 표가 엉뚱한 DB 에 만들어집니다: %q", lines[1])
	}

	// 문서 기본값을 물려받은 표와, 따로 정한 표.
	if !strings.Contains(sql, "ENGINE=InnoDB") {
		t.Errorf("문서 기본값이 DDL 에 없습니다:\n%s", sql)
	}
	if !strings.Contains(sql, "ENGINE=MyISAM") {
		t.Errorf("표별 설정이 DDL 에 없습니다:\n%s", sql)
	}
	if !strings.Contains(sql, "DEFAULT CHARSET=utf8mb4") {
		t.Errorf("문자셋이 DDL 에 없습니다:\n%s", sql)
	}

	// 되돌리기에 DROP DATABASE 는 없다.
	down, _ := body["downSql"].(string)
	if strings.Contains(strings.ToUpper(down), "DROP DATABASE") {
		t.Errorf("되돌리기가 데이터베이스를 통째로 지웁니다:\n%s", down)
	}
	plan, _ := body["plan"].(map[string]any)
	if irr, _ := plan["irreversible"].([]any); len(irr) == 0 {
		t.Error("되돌릴 수 없다는 표시가 없습니다")
	}
}

// 서버가 화면에 설정 목록을 내려 준다. 화면이 스스로 목록을 들고 있으면 DDL 을
// 만드는 쪽과 갈라진다.
func TestMetaCarriesStorageOptionSpecs(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("GET", "/api/v1/meta", nil)
	if status != 200 {
		t.Fatalf("meta = %d", status)
	}
	kinds, _ := body["dbKinds"].([]any)
	seen := map[string]map[string]any{}
	for _, it := range kinds {
		m, _ := it.(map[string]any)
		k, _ := m["kind"].(string)
		seen[k] = m
	}
	mysql := seen["mysql"]
	if mysql == nil {
		t.Fatal("mysql 이 목록에 없습니다")
	}
	if opts, _ := mysql["tableOptions"].([]any); len(opts) == 0 {
		t.Error("MySQL 표 설정 목록이 비었습니다")
	}
	if opts, _ := mysql["databaseOptions"].([]any); len(opts) == 0 {
		t.Error("MySQL 데이터베이스 설정 목록이 비었습니다")
	}
	// 커넥션 폼에도 세션 속성(시간대·문자셋)이 나와야 한다.
	hints, _ := mysql["optionHints"].([]any)
	keys := []string{}
	for _, hv := range hints {
		m, _ := hv.(map[string]any)
		k, _ := m["key"].(string)
		keys = append(keys, k)
	}
	for _, want := range []string{"timezone", "charset"} {
		found := false
		for _, k := range keys {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("커넥션 옵션에 %s 가 없습니다: %v", want, keys)
		}
	}
	if opts, _ := seen["sqlite"]["tableOptions"].([]any); len(opts) != 0 {
		t.Error("SQLite 에 표 설정 칸이 생겼습니다")
	}
}

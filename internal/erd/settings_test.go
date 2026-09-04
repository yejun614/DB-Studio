package erd

import "testing"

// 문서 기본값은 **새로 만드는 표에만** 들어간다. 이미 있는 표를 함께 바꾸면
// 표마다 일부러 다르게 정해 둔 값이 소리 없이 사라진다.
func TestTableDefaultsApplyToNewTablesOnly(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"before"}`)

	apply(t, doc, OpDocOptions, `{"tableDefaults":{"engine":"InnoDB","charset":"utf8mb4"}}`)
	apply(t, doc, OpTableAdd, `{"name":"after"}`)

	if got := doc.findTable("before").Options["engine"]; got != "" {
		t.Errorf("기본값을 정하기 전에 있던 표까지 바뀌었습니다: engine = %q", got)
	}
	after := doc.findTable("after")
	if after.Options["engine"] != "InnoDB" || after.Options["charset"] != "utf8mb4" {
		t.Errorf("새 표가 기본값을 물려받지 못했습니다: %v", after.Options)
	}

	// 표마다 다르게 정할 수 있다.
	apply(t, doc, OpTableUpdate, `{"key":"after","options":{"engine":"MyISAM"}}`)
	if got := doc.findTable("after").Options["engine"]; got != "MyISAM" {
		t.Errorf("표별 설정이 먹지 않습니다: %q", got)
	}
	// 그 뒤에 만든 표는 여전히 문서 기본값을 따른다.
	apply(t, doc, OpTableAdd, `{"name":"later"}`)
	if got := doc.findTable("later").Options["engine"]; got != "InnoDB" {
		t.Errorf("표별 설정이 문서 기본값을 덮어썼습니다: %q", got)
	}
}

// 이미 있는 표에 적용하는 것은 사람이 따로 고른다.
func TestTableDefaultsApplyToExisting(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"users"}`)
	apply(t, doc, OpTableUpdate, `{"key":"users","options":{"engine":"MyISAM"}}`)

	apply(t, doc, OpDocOptions,
		`{"tableDefaults":{"engine":"InnoDB"},"applyToTables":true}`)
	if got := doc.findTable("users").Options["engine"]; got != "InnoDB" {
		t.Errorf("모두에 적용이 먹지 않습니다: %q", got)
	}
}

// 빈 값은 그 열쇠를 지운다. 지운 열쇠가 이미 있는 표에서까지 사라지면
// "기본값 목록에서 빼기"가 표를 고치는 동작이 되어버린다.
func TestTableDefaultsRemoveKey(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpDocOptions, `{"tableDefaults":{"engine":"InnoDB"}}`)
	apply(t, doc, OpTableAdd, `{"name":"users"}`)
	apply(t, doc, OpDocOptions, `{"tableDefaults":{"engine":""},"applyToTables":true}`)

	if len(doc.TableDefaults) != 0 {
		t.Errorf("기본값이 지워지지 않았습니다: %v", doc.TableDefaults)
	}
	if got := doc.findTable("users").Options["engine"]; got != "InnoDB" {
		t.Errorf("기본값에서 뺐다고 표의 설정까지 사라졌습니다: %q", got)
	}
}

// 설정 값은 DDL 에 따옴표 없이 들어간다. 값 하나가 문장 구조를 바꾸는 길을 막는다.
func TestDocOptionValuesAreRestricted(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	for _, bad := range []string{
		`{"tableDefaults":{"engine":"InnoDB; DROP TABLE users"}}`,
		`{"tableDefaults":{"engine":"a b"}}`,
		`{"tableDefaults":{"engine":"x\"y"}}`,
	} {
		applyErr(t, doc, OpDocOptions, bad, "invalid", "쓸 수 없는 문자")
	}
	applyErr(t, doc, OpDocOptions, `{}`, "invalid", "바꿀 설정이 없습니다")
}

// 새 DB 계획은 설계 단계에서 아무것도 만들지 않는다. 만드는 문장만 적어 둔다.
func TestTargetDatabasePlan(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpDocOptions,
		`{"targetDb":{"name":"shop","options":{"charset":"utf8mb4","collation":"utf8mb4_0900_ai_ci"}}}`)

	if doc.TargetDB == nil || doc.TargetDB.Name != "shop" {
		t.Fatalf("대상 DB 계획이 없습니다: %+v", doc.TargetDB)
	}
	want := "CREATE DATABASE IF NOT EXISTS `shop` DEFAULT CHARACTER SET utf8mb4 " +
		"DEFAULT COLLATE utf8mb4_0900_ai_ci"
	if got := doc.CreateDatabaseSQL(); got != want {
		t.Errorf("CREATE DATABASE = %q\n기대값 %q", got, want)
	}
	if got := doc.UseDatabaseSQL(); got != "USE `shop`" {
		t.Errorf("USE = %q", got)
	}

	// 이름을 비우면 계획을 지운다.
	apply(t, doc, OpDocOptions, `{"targetDb":{"name":""}}`)
	if doc.TargetDB != nil {
		t.Errorf("계획이 지워지지 않았습니다: %+v", doc.TargetDB)
	}
	applyErr(t, doc, OpDocOptions, `{"targetDb":{"options":{"charset":"utf8mb4"}}}`,
		"invalid", "이름을 먼저")
}

// PostgreSQL 은 한 세션 안에서 데이터베이스를 옮겨 갈 수 없다. 그 사실을
// UseDatabaseSQL 이 빈 문자열로 말한다 — 이것을 모르면 나머지 문장이 접속해 있던
// 데이터베이스에 표를 만들어 놓고 성공했다고 보고한다.
func TestPostgresCannotSwitchDatabase(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "postgres")
	apply(t, doc, OpDocOptions, `{"targetDb":{"name":"shop","options":{"encoding":"UTF8"}}}`)
	if got := doc.CreateDatabaseSQL(); got != `CREATE DATABASE "shop" ENCODING 'UTF8'` {
		t.Errorf("CREATE DATABASE = %q", got)
	}
	if got := doc.UseDatabaseSQL(); got != "" {
		t.Errorf("PostgreSQL 에 옮겨 가는 문장이 생겼습니다: %q", got)
	}
}

// 사본에 적용한 op 가 원본의 설정을 바꾸면 안 된다(적용은 늘 사본에서 시작한다).
func TestCloneIsolatesSettings(t *testing.T) {
	doc := NewDocument("doc1", "테스트", "conn1", "mysql")
	apply(t, doc, OpDocOptions,
		`{"tableDefaults":{"engine":"InnoDB"},"targetDb":{"name":"shop"}}`)

	clone := doc.Clone()
	apply(t, clone, OpDocOptions,
		`{"tableDefaults":{"engine":"MyISAM"},"targetDb":{"name":"other"}}`)

	if got := doc.TableDefaults["engine"]; got != "InnoDB" {
		t.Errorf("사본을 고쳤는데 원본 기본값이 바뀌었습니다: %q", got)
	}
	if got := doc.TargetDB.Name; got != "shop" {
		t.Errorf("사본을 고쳤는데 원본 대상 DB 가 바뀌었습니다: %q", got)
	}
}

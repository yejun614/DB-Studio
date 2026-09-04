package erd

import (
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// 문서 수준 설정: 표의 기본 저장 설정과, 아직 없는 대상 데이터베이스.

// maxDocOptionValue는 설정 값 하나의 길이 제한이다. 엔진 이름이나 문자셋은 길어야
// 서른 자 남짓이고, 이것을 열어 두면 문서에 아무 글이나 담기는 칸이 하나 생긴다.
const maxDocOptionValue = 128

// TargetDB는 "아직 만들지 않은 데이터베이스"를 대상으로 삼는 계획이다.
//
// 설계 단계에서 DB를 만들지 않는 이유: 초안은 지우고 다시 그리는 물건이고, 그릴
// 때마다 서버에 빈 데이터베이스가 하나씩 쌓이면 아무도 그것을 치우지 않는다.
// 여기 적어 둔 것은 **마이그레이션이 실행될 때** 첫 문장(CREATE DATABASE)이 된다.
type TargetDB struct {
	// Name은 만들 데이터베이스 이름이다.
	Name string `json:"name"`
	// Options는 CREATE DATABASE 에 붙일 값이다(문자셋·인코딩·정렬 규칙 등).
	Options map[string]string `json:"options,omitempty"`
}

func (t *TargetDB) clone() *TargetDB {
	if t == nil {
		return nil
	}
	out := *t
	out.Options = cloneKV(t.Options)
	return &out
}

func cloneKV(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type docOptionsPayload struct {
	// TableDefaults는 새 표가 물려받을 기본 설정의 패치다(빈 값 = 그 열쇠를 지운다).
	TableDefaults *KVPatch `json:"tableDefaults,omitempty"`
	// ApplyToTables는 바뀐 기본값을 **이미 있는 표에도** 적어 넣는다.
	//
	// 따로 받는 이유: 기본값을 고치면 이미 있는 표까지 조용히 따라 바뀌는 편이
	// 편해 보이지만, 그것은 표마다 일부러 다르게 정해 둔 값을 소리 없이 지운다.
	// 두 가지는 서로 다른 뜻이므로 사람이 고르게 한다.
	ApplyToTables bool `json:"applyToTables,omitempty"`

	// TargetDB는 만들 데이터베이스다. 이름이 빈 문자열이면 계획을 지운다.
	TargetDB *targetDBPatch `json:"targetDb,omitempty"`
}

type targetDBPatch struct {
	Name    *string  `json:"name,omitempty"`
	Options *KVPatch `json:"options,omitempty"`
}

func applyDocOptions(doc *Document, op *Op) error {
	var p docOptionsPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	if p.TableDefaults == nil && p.TargetDB == nil && !p.ApplyToTables {
		return invalid("바꿀 설정이 없습니다")
	}
	if p.TableDefaults != nil {
		next, err := patchKV(doc.TableDefaults, *p.TableDefaults)
		if err != nil {
			return err
		}
		doc.TableDefaults = next
	}
	if p.ApplyToTables {
		for _, t := range doc.Schema.Tables {
			// 기본값이 있는 열쇠만 덮어쓴다. 지운 열쇠까지 표에서 지우면
			// "기본값 목록에서 빼기"가 표마다의 설정을 지우는 동작이 된다.
			for k, v := range doc.TableDefaults {
				if t.Options == nil {
					t.Options = map[string]string{}
				}
				t.Options[k] = v
			}
		}
	}
	if p.TargetDB != nil {
		if err := applyTargetDB(doc, p.TargetDB); err != nil {
			return err
		}
	}
	return nil
}

func applyTargetDB(doc *Document, p *targetDBPatch) error {
	cur := doc.TargetDB.clone()
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			// 이름을 지우는 것은 "새 DB 를 만들지 않는다"는 뜻이다.
			doc.TargetDB = nil
			return nil
		}
		if _, err := validateIdent("데이터베이스", name); err != nil {
			return err
		}
		if cur == nil {
			cur = &TargetDB{}
		}
		cur.Name = name
	}
	if cur == nil {
		return invalid("만들 데이터베이스 이름을 먼저 정하세요")
	}
	if p.Options != nil {
		next, err := patchKV(cur.Options, *p.Options)
		if err != nil {
			return err
		}
		cur.Options = next
	}
	doc.TargetDB = cur
	return nil
}

// patchKV는 문자열 맵에 패치를 적용한다(빈 값 = 삭제).
func patchKV(base map[string]string, patch KVPatch) (map[string]string, error) {
	out := cloneKV(base)
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range patch {
		key := strings.TrimSpace(k)
		if key == "" {
			return nil, invalid("설정 이름이 비어 있습니다")
		}
		v = strings.TrimSpace(v)
		if v == "" {
			delete(out, key)
			continue
		}
		if len([]rune(v)) > maxDocOptionValue {
			return nil, invalid("%s 설정 값이 너무 깁니다 (%d자 제한)", key, maxDocOptionValue)
		}
		// 설정 값은 DDL 에 따옴표 없이 그대로 들어간다(ENGINE=InnoDB). 식별자와
		// 같은 문자만 허용해서, 값 하나가 문장 구조를 바꾸는 길을 막는다.
		if strings.ContainsAny(v, badIdentChars) || strings.ContainsAny(v, " ,()=") {
			return nil, invalid("%s 설정 값에 쓸 수 없는 문자가 있습니다: %s", key, v)
		}
		out[key] = v
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// applyTableDefaults는 새로 생긴 표에 문서 기본값을 채운다.
//
// **비어 있는 열쇠만** 채운다. 표가 스스로 정한 값이 있으면 그것이 이긴다 — SQL
// 을 가져올 때 원본에 적힌 엔진이 문서 기본값에 덮이면, 가져온 스크립트와 화면이
// 서로 다른 말을 하게 된다.
func applyTableDefaults(doc *Document, tables ...*schema.Table) {
	if len(doc.TableDefaults) == 0 {
		return
	}
	for _, t := range tables {
		if t == nil {
			continue
		}
		for k, v := range doc.TableDefaults {
			if t.Options[k] != "" {
				continue
			}
			if t.Options == nil {
				t.Options = map[string]string{}
			}
			t.Options[k] = v
		}
	}
}

// CreateDatabaseSQL은 대상 데이터베이스를 만드는 문장이다(계획이 없으면 빈 문자열).
//
// 내보내는 SQL 과 마이그레이션의 **맨 앞**에 붙는다. 설계 단계에서는 아무것도
// 만들지 않으므로, 이 문장이 실행되는 순간이 그 DB 가 생기는 순간이다.
func (d *Document) CreateDatabaseSQL() string {
	if d == nil || d.TargetDB == nil || strings.TrimSpace(d.TargetDB.Name) == "" {
		return ""
	}
	name := d.TargetDB.Name
	opt := d.TargetDB.Options
	switch strings.ToLower(strings.TrimSpace(d.Dialect)) {
	case "mysql":
		b := &strings.Builder{}
		fmt.Fprintf(b, "CREATE DATABASE IF NOT EXISTS `%s`", name)
		if v := opt["charset"]; v != "" {
			fmt.Fprintf(b, " DEFAULT CHARACTER SET %s", v)
		}
		if v := opt["collation"]; v != "" {
			fmt.Fprintf(b, " DEFAULT COLLATE %s", v)
		}
		return b.String()
	case "postgres":
		// PostgreSQL 에는 IF NOT EXISTS 가 없다. 이미 있으면 실패하는데, 그것이
		// 조용히 남의 DB 에 이어 붙이는 것보다 낫다.
		b := &strings.Builder{}
		fmt.Fprintf(b, `CREATE DATABASE "%s"`, name)
		if v := opt["encoding"]; v != "" {
			fmt.Fprintf(b, " ENCODING '%s'", v)
		}
		if v := opt["lc_collate"]; v != "" {
			fmt.Fprintf(b, " LC_COLLATE '%s'", v)
		}
		if v := opt["lc_ctype"]; v != "" {
			fmt.Fprintf(b, " LC_CTYPE '%s'", v)
		}
		if v := opt["template"]; v != "" {
			fmt.Fprintf(b, " TEMPLATE %s", v)
		}
		return b.String()
	case "mssql":
		b := &strings.Builder{}
		fmt.Fprintf(b, "IF DB_ID('%s') IS NULL CREATE DATABASE [%s]", name, name)
		if v := opt["collation"]; v != "" {
			fmt.Fprintf(b, " COLLATE %s", v)
		}
		return b.String()
	case "sqlite":
		// 파일 하나가 곧 데이터베이스라, 만드는 문장이 없다.
		return ""
	default:
		return fmt.Sprintf("CREATE DATABASE %s", name)
	}
}

// UseDatabaseSQL은 방금 만든 데이터베이스로 옮겨 가는 문장이다.
//
// 빈 문자열이면 **한 세션 안에서 옮겨 갈 수 없다는 뜻**이다(PostgreSQL·Oracle).
// 그 DB 들에서는 CREATE DATABASE 를 다른 데이터베이스에 붙어서 실행한 뒤 접속을
// 새로 열어야 한다. 이 구분이 없으면 나머지 문장이 엉뚱한 데이터베이스에 표를
// 만들어 놓고도 성공했다고 보고한다.
func (d *Document) UseDatabaseSQL() string {
	if d == nil || d.TargetDB == nil || strings.TrimSpace(d.TargetDB.Name) == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(d.Dialect)) {
	case "mysql":
		return fmt.Sprintf("USE `%s`", d.TargetDB.Name)
	case "mssql":
		return fmt.Sprintf("USE [%s]", d.TargetDB.Name)
	default:
		return ""
	}
}

package schema

import "testing"

// 카탈로그의 값어치는 "고른 타입을 이 앱이 다시 이해하는가"에 있다.
// 목록에만 있고 파서가 모르는 타입은 화면에서 고르는 순간 unknown이 되어,
// 다른 DB로 옮길 때 그대로 문자열로 새어 나간다.

func TestCatalogTypesParseBack(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql", "mssql", "oracle", "sqlite"} {
		cat := TypeCatalog(dialect)
		if len(cat.Types) == 0 {
			t.Fatalf("%s: 카탈로그가 비어 있습니다", dialect)
		}
		for _, def := range cat.Types {
			raw := def.Name
			switch def.Param {
			case ParamLength, ParamFraction:
				raw += "(" + firstNumber(def.Default) + ")"
			case ParamPrecision:
				raw += "(" + def.Default + ")"
			case ParamValues:
				raw += "('a','b')"
			}
			got := ParseType(cat.Dialect, raw)
			if got.Base == TypeUnknown && !knownUnknown[def.Name] {
				t.Errorf("%s: %s 를 파서가 모릅니다 (raw=%q)", dialect, def.Name, raw)
			}
		}
	}
}

// knownUnknown은 논리 타입으로 정규화하지 않기로 한 타입이다.
//
// 이 목록을 두는 이유: 이런 타입은 다른 DB에 대응이 없어 억지로 정규화하면 옮길 때
// 엉뚱한 타입이 된다. 원본 문자열을 그대로 지키는 편이 맞고(RenderType의 preferRaw),
// 그 판단이 의도된 것임을 시험으로 못 박는다.
var knownUnknown = map[string]bool{
	// PostgreSQL 전용
	"HSTORE": true, "LINE": true, "LSEG": true, "BOX": true, "PATH": true,
	"POLYGON": true, "CIRCLE": true, "INT4RANGE": true, "NUMRANGE": true,
	"TSRANGE": true, "TSTZRANGE": true, "DATERANGE": true, "TSVECTOR": false,
	// MySQL 공간 타입
	"LINESTRING": true, "MULTIPOINT": true, "MULTILINESTRING": true,
	"MULTIPOLYGON": true, "GEOMETRYCOLLECTION": true,
	// MS-SQL
	"NVARCHAR(MAX)": true, "VARCHAR(MAX)": true, "VARBINARY(MAX)": true,
	"HIERARCHYID": true, "SQL_VARIANT": true, "ROWVERSION": true,
	// Oracle
	"INTERVAL YEAR TO MONTH": true, "INTERVAL DAY TO SECOND": true,
	"BFILE": true, "LONG RAW": true,
}

func firstNumber(def string) string {
	for i, r := range def {
		if r == ',' {
			return def[:i]
		}
	}
	if def == "" {
		return "1"
	}
	return def
}

func TestCatalogShape(t *testing.T) {
	cat := TypeCatalog("mysql")
	byName := map[string]TypeDef{}
	for _, d := range cat.Types {
		byName[d.Name] = d
		if d.Label == "" || d.Category == "" {
			t.Errorf("%s: 라벨/분류가 비어 있습니다", d.Name)
		}
		if d.Param == ParamPrecision && d.Default == "" {
			t.Errorf("%s: 자릿수 기본값이 없습니다", d.Name)
		}
	}
	if !byName["INT"].Unsigned {
		t.Error("MySQL의 INT는 UNSIGNED를 붙일 수 있어야 합니다")
	}
	if byName["VARCHAR"].Param != ParamLength {
		t.Error("VARCHAR는 길이를 받아야 합니다")
	}
	if byName["ENUM"].Param != ParamValues {
		t.Error("ENUM은 값 목록을 받아야 합니다")
	}
	if cat.Arrays {
		t.Error("MySQL에는 배열 타입이 없습니다")
	}
	if !TypeCatalog("postgres").Arrays {
		t.Error("PostgreSQL은 배열을 지원합니다")
	}
	// 자동 증가는 DB마다 이름과 조건이 다르다. 화면이 그 차이를 설명하려면
	// 카탈로그가 그 사실을 함께 알려줘야 한다.
	for _, d := range []string{"postgres", "mysql", "mssql", "oracle", "sqlite"} {
		if TypeCatalog(d).AutoIncrement == "" {
			t.Errorf("%s: 자동 증가 이름이 비어 있습니다", d)
		}
	}
}

func TestUnknownDialectStillUsable(t *testing.T) {
	// 모르는 dialect에도 최소 목록은 준다. 빈 목록을 주면 화면에 고를 것이 없어
	// 직접 입력 말고는 길이 없다.
	cat := TypeCatalog("duckdb")
	if len(cat.Types) == 0 {
		t.Fatal("공통 타입 목록이 비어 있습니다")
	}
}

// TestIdentityTypes는 자동 증가를 붙일 수 있는 타입 표시를 확인한다.
//
// 이 표시가 화면의 유일한 근거다. 화면은 타입 이름을 보고 짐작하지 않고 이 값만
// 보므로, 여기서 빠지면 그 DB에서는 자동 증가를 켤 방법이 사라진다.
func TestIdentityTypes(t *testing.T) {
	want := map[string][]string{
		"postgres": {"SMALLINT", "INTEGER", "BIGINT"},
		"mysql":    {"TINYINT", "SMALLINT", "MEDIUMINT", "INT", "BIGINT"},
		// MS-SQL은 소수 자릿수가 0인 DECIMAL에도 붙는다. 자릿수 조건은 인자에 달린
		// 것이라 타입 표시로는 나타낼 수 없고, 화면이 한 번 더 본다.
		"mssql":  {"TINYINT", "SMALLINT", "INT", "BIGINT", "DECIMAL", "NUMERIC"},
		"oracle": {"NUMBER", "INTEGER"},
		// SQLite의 AUTOINCREMENT는 INTEGER PRIMARY KEY 컬럼에만 붙는다.
		"sqlite": {"INTEGER"},
	}
	for dialect, names := range want {
		cat := TypeCatalog(dialect)
		got := map[string]bool{}
		for _, ty := range cat.Types {
			if ty.Identity {
				got[ty.Name] = true
			}
		}
		for _, n := range names {
			if !got[n] {
				t.Errorf("%s: %s 에 자동 증가를 붙일 수 있어야 합니다", dialect, n)
			}
			delete(got, n)
		}
		for n := range got {
			t.Errorf("%s: %s 는 정수 계열이 아닌데 자동 증가로 표시됐습니다", dialect, n)
		}
	}

	// 문자·시간 타입에는 어느 DB에서도 붙지 않는다.
	for _, d := range []string{"postgres", "mysql", "mssql", "oracle", "sqlite"} {
		for _, ty := range TypeCatalog(d).Types {
			if ty.Identity && (ty.Category == CatText || ty.Category == CatTime) {
				t.Errorf("%s: %s(%s)에 자동 증가가 표시됐습니다", d, ty.Name, ty.Category)
			}
		}
	}
}

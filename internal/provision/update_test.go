package provision

import (
	"strings"
	"testing"
)

// 살아 있는 컨테이너로 잰 것을 검사로 박아 둔다.
//
// ── 왜 이 검사가 있는가 ─────────────────────────────────────────────
// 이 표는 짐작으로 채울 수 없다. 일곱 종류를 다 띄워서 비밀번호를 고치고 다시
// 만들어 봤더니 **DB 마다 달랐다.** 그 결과가 화면의 안내 문구가 되므로, 누가
// 나중에 "정리"하다 바꾸면 화면이 사람에게 거짓을 말하게 된다.
//
//	2026-09-10 측정:
//	  PostgreSQL · MySQL · MariaDB · MongoDB · MS-SQL → 옛 비밀번호가 남는다
//	  ClickHouse · Redis                              → 새 비밀번호가 먹는다
func TestApplyMatchesWhatWeMeasured(t *testing.T) {
	cases := []struct {
		recipe, field string
		want          Apply
	}{
		// 첫 실행에서만 쓰는 것들(고쳐도 지금 DB 는 그대로다).
		{"postgres", "password", ApplyInitOnly},
		{"postgres", "database", ApplyInitOnly},
		{"postgres", "username", ApplyInitOnly},
		{"mysql", "password", ApplyInitOnly},
		{"mariadb", "password", ApplyInitOnly},
		{"mongodb", "password", ApplyInitOnly},
		{"mssql", "password", ApplyInitOnly},
		// 다시 만들면 실제로 바뀌는 것들.
		{"clickhouse", "password", ApplyRecreate},
		{"redis", "password", ApplyRecreate},
		// 목적지에서 뽑히는 것들.
		{"postgres", "memoryMB", ApplyLive},
		{"postgres", "cpus", ApplyLive},
		{"postgres", "restart", ApplyLive},
		{"postgres", "hostPort", ApplyRecreate},
		{"postgres", "persist", ApplyRecreate},
		{"postgres", "maxConnections", ApplyRecreate},
		{"clickhouse", "markCacheMB", ApplyRestart},
		{"clickhouse", "maxQueryMemoryMB", ApplyRestart},
	}
	for _, tc := range cases {
		r := Find(tc.recipe)
		if r == nil {
			t.Fatalf("%s 레시피가 없습니다", tc.recipe)
		}
		f := r.Field(tc.field)
		if f == nil {
			t.Errorf("%s 에 %s 칸이 없습니다", tc.recipe, tc.field)
			continue
		}
		if got := ApplyOf(f); got != tc.want {
			t.Errorf("%s/%s = %s (기대 %s)", tc.recipe, tc.field, got, tc.want)
		}
	}
}

// 모든 비밀 칸은 무엇이 필요한지가 **정해져 있어야** 한다.
//
// 비어 있으면 목적지에서 뽑는데, 비밀번호는 목적지로 정해지지 않는다(같은
// 환경변수라도 DB 마다 다르다). 새 DB 를 더할 때 이 자리를 비워 두면 화면이
// "다시 만들면 바뀝니다"라고 말하는데 실제로는 안 바뀌는 일이 생긴다.
func TestEverySecretFieldSaysHowItApplies(t *testing.T) {
	for _, r := range Catalog() {
		for i := range r.Fields {
			f := &r.Fields[i]
			if !f.Secret || f.Hidden {
				continue
			}
			if f.Apply == "" {
				t.Errorf("%s/%s: 비밀 칸인데 Apply 가 비어 있습니다 — "+
					"살아 있는 컨테이너로 재서 적으세요", r.ID, f.Key)
			}
		}
	}
}

// 차이는 준 칸만 본다.
//
// 안 준 칸을 "지우라"로 읽으면, 화면이 일부만 보낸 요청 하나로 설정이 통째로
// 날아간다.
func TestDiffOnlyLooksAtWhatWasGiven(t *testing.T) {
	r := Find("postgres")
	before := map[string]string{"database": "appdb", "memoryMB": "512", "password": "pw"}

	// 아무것도 안 주면 아무 차이도 없다.
	if got := Diff(r, before, map[string]string{}); len(got) != 0 {
		t.Errorf("빈 요청에 차이가 %d개: %+v", len(got), got)
	}
	// 같은 값을 주면 차이가 아니다.
	if got := Diff(r, before, map[string]string{"memoryMB": "512"}); len(got) != 0 {
		t.Errorf("같은 값에 차이가 났습니다: %+v", got)
	}
	// 하나만 주면 하나만 바뀐다.
	got := Diff(r, before, map[string]string{"memoryMB": "1024"})
	if len(got) != 1 || got[0].Key != "memoryMB" || got[0].To != "1024" {
		t.Fatalf("%+v", got)
	}
	if got[0].Apply != ApplyLive {
		t.Errorf("메모리 상한 = %s (기대 live)", got[0].Apply)
	}
}

// 비밀 값은 차이에도 실리지 않는다.
//
// 이 목록은 화면에 그대로 보이고 감사 기록에도 남는다.
func TestDiffHidesSecretValues(t *testing.T) {
	r := Find("postgres")
	before := map[string]string{"password": "옛-비밀번호"}
	got := Diff(r, before, map[string]string{"password": "새-비밀번호"})
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(got[0].From+got[0].To, "비밀번호-") ||
		got[0].From == "옛-비밀번호" || got[0].To == "새-비밀번호" {
		t.Errorf("비밀번호가 그대로 실렸습니다: %+v", got[0])
	}
	if got[0].Apply != ApplyInitOnly {
		t.Errorf("PostgreSQL 비밀번호 = %s (기대 init)", got[0].Apply)
	}
}

// 필요한 것은 가장 무거운 것이고, init-only 는 무게에 넣지 않는다.
func TestNeededPicksTheHeaviest(t *testing.T) {
	cases := []struct {
		name string
		in   []Change
		want Apply
	}{
		{"없음", nil, ApplyInitOnly},
		{"init 만", []Change{{Apply: ApplyInitOnly}}, ApplyInitOnly},
		{"live 만", []Change{{Apply: ApplyLive}}, ApplyLive},
		{"live + init", []Change{{Apply: ApplyLive}, {Apply: ApplyInitOnly}}, ApplyLive},
		{"restart + live", []Change{{Apply: ApplyRestart}, {Apply: ApplyLive}}, ApplyRestart},
		{"recreate 가 섞이면", []Change{
			{Apply: ApplyLive}, {Apply: ApplyRecreate}, {Apply: ApplyRestart},
		}, ApplyRecreate},
	}
	for _, tc := range cases {
		if got := Needed(tc.in); got != tc.want {
			t.Errorf("%s: %s (기대 %s)", tc.name, got, tc.want)
		}
	}

	// init-only 만 바뀌었으면 그것만 따로 뽑혀야 한다.
	only := InitOnly([]Change{
		{Key: "password", Apply: ApplyInitOnly}, {Key: "memoryMB", Apply: ApplyLive},
	})
	if len(only) != 1 || only[0].Key != "password" {
		t.Errorf("%+v", only)
	}
}

// 안내 문구는 비어 있으면 안 된다. 화면이 그대로 보여 준다.
func TestDescribeNeedSaysSomething(t *testing.T) {
	for _, a := range []Apply{ApplyLive, ApplyRestart, ApplyRecreate, ApplyInitOnly} {
		if strings.TrimSpace(DescribeNeed(a)) == "" {
			t.Errorf("%s 에 대한 안내가 없습니다", a)
		}
	}
}

// 카탈로그로 나가는 필드에는 **언제나** Apply 가 들어 있어야 한다.
//
// 비워서 내보내면 화면이 스스로 짐작하게 되고, 그러면 같은 규칙이 두 곳에
// 생긴다. 실제로 어긋났다 — 메모리 상한이 화면에서 "다시 만들기 필요"로
// 보였는데 진짜는 "바로 적용"이었다.
func TestCatalogAlwaysCarriesApply(t *testing.T) {
	for _, r := range Catalog() {
		for i := range r.Fields {
			f := &r.Fields[i]
			if f.Apply == "" {
				t.Errorf("%s/%s: Apply 가 비어 나갑니다", r.ID, f.Key)
			}
		}
	}
	// 값도 맞아야 한다(비어 있지 않기만 하면 되는 것이 아니다).
	pg := Find("postgres")
	if got := pg.Field("memoryMB").Apply; got != ApplyLive {
		t.Errorf("메모리 상한 = %s (기대 live)", got)
	}
	if got := pg.Field("password").Apply; got != ApplyInitOnly {
		t.Errorf("PostgreSQL 비밀번호 = %s (기대 init)", got)
	}
}

// 적어 두지 않은 칸의 지금 값은 기본값이다.
//
// 빈 값으로 보면 화면이 칸을 채워 보낼 때마다 "안 정한 것 → 기본값"이 변경으로
// 잡힌다. 메모리 상한 하나를 고쳤을 뿐인데 손대지 않은 칸이 함께 바뀐 것이 되고,
// 그중 하나가 다시 만들기를 요구하면 컨테이너가 괜히 다시 만들어진다 —
// 브라우저로 눌러 보다 실제로 그렇게 되는 것을 봤다.
func TestDiffTreatsUnsetAsTheDefault(t *testing.T) {
	r := Find("postgres")
	// 저장된 값에는 maxConnections·restart 가 없다(만들 때 정하지 않았다).
	before := map[string]string{"database": "appdb", "memoryMB": "512"}
	// 화면은 칸을 전부 채워 보낸다 — 기본값 그대로인 것까지.
	after := map[string]string{
		"memoryMB":       "1024",
		"maxConnections": r.Field("maxConnections").Default,
		"restart":        r.Field("restart").Default,
	}

	got := Diff(r, before, after)
	if len(got) != 1 || got[0].Key != "memoryMB" {
		keys := []string{}
		for _, c := range got {
			keys = append(keys, c.Key)
		}
		t.Fatalf("바뀐 것 = %v (기대 memoryMB 하나)", keys)
	}
	if need := Needed(got); need != ApplyLive {
		t.Errorf("필요한 것 = %s (기대 live — 다시 만들 이유가 없다)", need)
	}

	// 기본값과 다른 값을 주면 그것은 진짜 변경이다.
	got = Diff(r, before, map[string]string{"maxConnections": "999"})
	if len(got) != 1 || got[0].From != r.Field("maxConnections").Default {
		t.Errorf("%+v", got)
	}
}

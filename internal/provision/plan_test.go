package provision

import (
	"strconv"
	"strings"
	"testing"
)

// spec은 검사용 최소 설정이다(필수 칸만 채운다).
func spec(kind string, extra map[string]string) Spec {
	vals := map[string]string{"password": "pw1234!"}
	switch kind {
	case "postgres", "mysql", "mongodb", "clickhouse":
		vals["database"] = "appdb"
	}
	if kind == "mongodb" {
		vals["username"] = "root"
	}
	for k, v := range extra {
		vals[k] = v
	}
	return Spec{Kind: kind, Name: "t1", Values: vals}
}

// 필수 값을 빠뜨리면 만들 수 없다.
//
// 이것을 서버에서 막는 이유: PostgreSQL 은 비밀번호 없이 **아예 뜨지 않는다.**
// 그대로 만들면 컨테이너가 생기고 곧 죽는데, 사람에게는 "만들었는데 안 돈다"로
// 보인다. 만들기 전에 막는 편이 정직하다.
func TestBuildRequiresPassword(t *testing.T) {
	_, err := Build(Spec{Kind: "postgres", Name: "t1", Values: map[string]string{"database": "appdb"}})
	if err == nil {
		t.Fatal("비밀번호 없이 계획이 만들어졌습니다")
	}
	if !strings.Contains(err.Error(), "비밀번호") {
		t.Errorf("어느 칸인지 말하지 않습니다: %v", err)
	}
}

// 값이 **어디로 가는지**를 지킨다.
//
// 이 검사가 필요한 이유: DB 마다 같은 뜻의 값이 다른 이름·다른 길로 간다.
// PostgreSQL 은 POSTGRES_DB 환경변수, MySQL 은 MYSQL_DATABASE, ClickHouse 는
// CLICKHOUSE_DB 다. 인자 모양도 다르다(-c 이름=값 / --이름=값 / --이름 값).
// 한 곳을 고치다 다른 쪽을 흘리면 컨테이너는 뜨고 설정만 조용히 빠진다.
func TestBuildRoutesValuesToTheRightPlace(t *testing.T) {
	cases := []struct {
		kind string
		vals map[string]string
		// wantEnv는 있어야 하는 환경변수다.
		wantEnv map[string]string
		// wantArg는 Cmd 에 **이 순서로 붙어** 있어야 하는 조각들이다.
		//
		// 낱개로 적는 것이 요점이다. 공백으로 이어 붙인 한 덩이를 확인하면
		// 원래 버그를 못 잡는다 — "-c max_connections=200" 을 argv 하나로
		// 넘기면 PostgreSQL 이 옵션 이름을 " max_connections"(앞에 공백)로
		// 읽고 뜨지 않는데, 문자열로 견주는 검사는 통과한다.
		wantArg [][]string
	}{
		{
			kind: "postgres",
			vals: map[string]string{"maxConnections": "200", "sharedBuffers": "256MB"},
			wantEnv: map[string]string{
				"POSTGRES_DB": "appdb", "POSTGRES_USER": "postgres",
				"POSTGRES_PASSWORD": "pw1234!",
			},
			wantArg: [][]string{
				{"-c", "max_connections=200"}, {"-c", "shared_buffers=256MB"},
			},
		},
		{
			kind:    "mysql",
			vals:    map[string]string{"maxConnections": "300", "charset": "utf8mb4"},
			wantEnv: map[string]string{"MYSQL_DATABASE": "appdb", "MYSQL_ROOT_PASSWORD": "pw1234!"},
			wantArg: [][]string{
				{"--max_connections=300"}, {"--character-set-server=utf8mb4"},
			},
		},
		{
			kind:    "redis",
			vals:    map[string]string{"maxmemoryPolicy": "allkeys-lru", "appendonly": "false"},
			wantEnv: map[string]string{},
			wantArg: [][]string{
				{"--requirepass", "pw1234!"}, {"--maxmemory-policy", "allkeys-lru"},
				{"--appendonly", "no"},
			},
		},
		{
			kind:    "mongodb",
			vals:    map[string]string{},
			wantEnv: map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": "pw1234!"},
		},
		{
			kind:    "clickhouse",
			vals:    map[string]string{},
			wantEnv: map[string]string{"CLICKHOUSE_DB": "appdb", "CLICKHOUSE_PASSWORD": "pw1234!"},
		},
	}

	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			p, err := Build(spec(c.kind, c.vals))
			if err != nil {
				t.Fatalf("계획을 만들지 못했습니다: %v", err)
			}
			for k, want := range c.wantEnv {
				if got := p.Env[k]; got != want {
					t.Errorf("%s = %q, 기대 %q (환경변수 전체: %v)", k, got, want, p.Env)
				}
			}
			for _, want := range c.wantArg {
				if !containsSeq(p.Args, want) {
					t.Errorf("인자에 %v 가 (붙어서) 없습니다: %v", want, p.Args)
				}
			}
		})
	}
}

// 비밀번호가 든 환경변수는 가릴 대상으로 표시돼야 한다.
//
// 표시가 없으면 감사 로그와 미리보기에 비밀번호가 그대로 남는다.
func TestBuildMarksSecrets(t *testing.T) {
	for _, kind := range []string{"postgres", "mysql", "mongodb", "clickhouse", "mssql"} {
		t.Run(kind, func(t *testing.T) {
			p, err := Build(spec(kind, nil))
			if err != nil {
				t.Fatalf("%v", err)
			}
			// 비밀번호가 환경변수로 가는 종류만 본다(Redis 는 인자로 간다).
			var pwEnv string
			for k, v := range p.Env {
				if v == "pw1234!" {
					pwEnv = k
				}
			}
			if pwEnv == "" {
				t.Skip("이 종류는 비밀번호를 환경변수로 넘기지 않는다")
			}
			if !contains(p.Secrets, pwEnv) {
				t.Errorf("%s 를 가릴 대상으로 표시하지 않았습니다: %v", pwEnv, p.Secrets)
			}
		})
	}
}

// MB 로 물은 값에는 단위를 붙여야 한다.
//
// mysql 의 innodb-buffer-pool-size 는 단위 없는 숫자를 **바이트**로 읽는다.
// 화면이 "MB" 라고 물었으므로 사람은 512 라고 적는데, 그대로 넘기면 512바이트가
// 되고 MySQL 이 뜨지 않는다. 뜨지 않는 이유는 로그 깊숙이에만 있다.
func TestBuildAddsSizeUnits(t *testing.T) {
	p, err := Build(spec("mysql", map[string]string{"innodbBufferPoolMB": "512"}))
	if err != nil {
		t.Fatal(err)
	}
	if !containsSeq(p.Args, []string{"--innodb-buffer-pool-size=512M"}) {
		t.Errorf("단위가 붙지 않았습니다: %v", p.Args)
	}
	// 이미 단위가 붙은 값은 그대로 둔다.
	//
	// 이 길로는 Build 가 오지 않는다(숫자 칸에 "1G" 를 넣을 수 없다). 그래도
	// 막아 두는 이유: 이 함수를 다른 자리에서 부르게 되는 날 두 번 붙이면
	// "512MM" 같은 값이 나가고, 그 실패는 DB 로그 깊숙이에만 남는다.
	f := &Field{Key: "innodbBufferPoolMB", Kind: KindNumber}
	if got := withUnit(f, "1G", "M"); got != "1G" {
		t.Errorf("이미 단위가 붙은 값을 %q 로 만들었습니다", got)
	}
	if got := withUnit(&Field{Key: "maxConnections"}, "200", "M"); got != "200" {
		t.Errorf("크기 값이 아닌데 단위를 붙였습니다: %q", got)
	}
}

// ClickHouse 의 설정은 파일로 가고, 서버 설정과 프로필 설정이 갈려야 한다.
//
// 프로필 값을 config.d 의 바깥에 적으면 **오류도 없이 그냥 안 먹는다.**
// 이 세션에 실제로 그렇게 어긋난 것을 고쳤다.
func TestBuildClickHouseConfigFile(t *testing.T) {
	p, err := Build(spec("clickhouse", map[string]string{
		"markCacheMB": "256", "maxQueryMemoryMB": "1000", "maxMemoryRatio": "0.5",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("파일이 %d개입니다: %v", len(p.Files), p.Files)
	}
	server, user := p.Files[ClickHouseServerConfig], p.Files[ClickHouseUserConfig]
	if server == "" || user == "" {
		t.Fatalf("설정 파일 자리가 어긋났습니다: %v", p.Files)
	}

	// 마크 캐시는 MB → 바이트로 바뀌어야 한다. ClickHouse 는 단위 없는 숫자를
	// 바이트로 읽는다.
	if !strings.Contains(server, "<mark_cache_size>268435456</mark_cache_size>") {
		t.Errorf("마크 캐시가 바이트로 적히지 않았습니다:\n%s", server)
	}
	// 질의 상한도 바이트다. 이름에 단위가 없어서 한 번 빠졌던 자리다 —
	// 1000MB 를 1000바이트로 적으면 모든 질의가 실패한다.
	if !strings.Contains(user, "<max_memory_usage>1048576000</max_memory_usage>") {
		t.Errorf("질의 상한이 바이트로 적히지 않았습니다:\n%s", user)
	}

	// 프로필 설정은 users.d 로 가야 한다. config.d 에 적으면 오류도 경고도 없이
	// 안 먹는다(살아 있는 서버에서 확인했다).
	if !strings.Contains(user, "<profiles>") {
		t.Errorf("프로필 설정이 profiles 안에 없습니다:\n%s", user)
	}
	if strings.Contains(server, "max_memory_usage") {
		t.Errorf("프로필 설정이 config.d 로 갔습니다:\n%s", server)
	}
	if strings.Contains(user, "max_server_memory_usage_to_ram_ratio") {
		t.Errorf("서버 설정이 users.d 로 갔습니다:\n%s", user)
	}
	if !strings.Contains(server, "max_server_memory_usage_to_ram_ratio") {
		t.Errorf("서버 설정이 config.d 에 없습니다:\n%s", server)
	}
}

// 이름은 좁게 막는다. 이 이름이 컨테이너·볼륨 이름이 된다.
func TestBuildRejectsBadNames(t *testing.T) {
	bad := []string{"", "T1", "1abc", "a b", "a/b", "../etc", "a_b", strings.Repeat("a", 41)}
	for _, name := range bad {
		s := spec("postgres", nil)
		s.Name = name
		if _, err := Build(s); err == nil {
			t.Errorf("%q 를 받아들였습니다", name)
		}
	}
	for _, name := range []string{"t1", "my-db", "a", "pg-2026"} {
		s := spec("postgres", nil)
		s.Name = name
		if _, err := Build(s); err != nil {
			t.Errorf("%q 를 거절했습니다: %v", name, err)
		}
	}
}

// 판본은 목록에 있는 것만 받는다.
func TestBuildRejectsUnknownVersion(t *testing.T) {
	s := spec("postgres", nil)
	s.Version = "8-alpine; rm -rf /"
	if _, err := Build(s); err == nil {
		t.Fatal("모르는 판본을 받아들였습니다")
	}
	s.Version = "16-alpine"
	p, err := Build(s)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if p.Image != "postgres:16-alpine" {
		t.Errorf("이미지 = %s", p.Image)
	}
}

// 줄바꿈이 든 값은 막는다. 인자 하나가 둘이 되고 환경변수는 그 자리에서 끊긴다.
func TestBuildRejectsNewlines(t *testing.T) {
	if _, err := Build(spec("postgres", map[string]string{"database": "a\nb"})); err == nil {
		t.Error("줄바꿈이 든 값을 받아들였습니다")
	}
}

// 목록에 없는 종류는 **왜 없는지**를 말해야 한다.
func TestBuildExplainsMissingKinds(t *testing.T) {
	for _, kind := range []string{"oracle", "sqlite", "kafka", "rabbitmq"} {
		_, err := Build(Spec{Kind: kind, Name: "t1"})
		if err == nil {
			t.Errorf("%s 로 계획이 만들어졌습니다", kind)
			continue
		}
		if !strings.Contains(err.Error(), "아직 만들 수 없습니다") {
			t.Errorf("%s: 이유를 말하지 않습니다: %v", kind, err)
		}
	}
	if _, err := Build(Spec{Kind: "쓰레기", Name: "t1"}); err == nil {
		t.Error("모르는 종류로 계획이 만들어졌습니다")
	}
}

// 도커 요청으로 옮길 때 잃는 것이 없어야 한다.
func TestCreateRequestCarriesEverything(t *testing.T) {
	p, err := Build(spec("postgres", map[string]string{
		"hostPort": "15432", "memoryMB": "512", "cpus": "1.5", "restart": "always",
	}))
	if err != nil {
		t.Fatal(err)
	}
	req := p.CreateRequest("inst-1")

	if req.Image != p.Image {
		t.Errorf("이미지 = %s", req.Image)
	}
	if got := req.HostConfig.PortBindings["5432/tcp"]; len(got) != 1 || got[0].HostPort != "15432" {
		t.Errorf("포트 = %+v", got)
	}
	if req.HostConfig.Memory != 512<<20 {
		t.Errorf("메모리 = %d", req.HostConfig.Memory)
	}
	if req.HostConfig.NanoCPUs != 1_500_000_000 {
		t.Errorf("CPU = %d", req.HostConfig.NanoCPUs)
	}
	if req.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("다시 시작 = %s", req.HostConfig.RestartPolicy.Name)
	}
	if len(req.HostConfig.Mounts) != 1 ||
		req.HostConfig.Mounts[0].Target != "/var/lib/postgresql/data" {
		t.Errorf("마운트 = %+v", req.HostConfig.Mounts)
	}
	// compose 라벨이 있어야 도커 CLI 에서 한 프로젝트로 보인다.
	if req.Labels[LabelProject] != ProjectName {
		t.Errorf("compose 프로젝트 라벨이 없습니다: %v", req.Labels)
	}
	if req.Labels[LabelInstance] != "inst-1" {
		t.Errorf("인스턴스 라벨 = %q", req.Labels[LabelInstance])
	}
	// 헬스체크가 없으면 "뜨는 중"과 "고장"을 구별할 수 없다.
	if req.Healthcheck == nil || len(req.Healthcheck.Test) == 0 {
		t.Error("헬스체크가 없습니다")
	}
	// 환경변수는 KEY=value 꼴이어야 한다.
	found := false
	for _, e := range req.Env {
		if e == "POSTGRES_PASSWORD=pw1234!" {
			found = true
		}
	}
	if !found {
		t.Errorf("환경변수 = %v", req.Env)
	}
	if req.NetworkingConfig == nil {
		t.Error("네트워크를 붙이지 않았습니다")
	}
}

// 데이터를 남기지 않거나 메모리 상한이 없으면 알려 줘야 한다.
//
// 막지는 않는다. 둘 다 정당한 선택이고(시험용 DB, 넉넉한 기계), 막으면 그
// 판단을 우리가 대신하는 셈이다. 다만 조용히 지나가면 안 된다.
func TestBuildWarnsAboutEphemeralAndUnlimited(t *testing.T) {
	p, err := Build(spec("postgres", map[string]string{"persist": "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Volume != "" {
		t.Errorf("볼륨을 만들었습니다: %s", p.Volume)
	}
	joined := strings.Join(p.Warnings, " ")
	for _, want := range []string{"데이터", "메모리"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q 경고가 없습니다: %v", want, p.Warnings)
		}
	}

	// 둘을 정하면 경고가 사라져야 한다. 늘 떠 있는 경고는 읽히지 않는다.
	p, err = Build(spec("postgres", map[string]string{"persist": "true", "memoryMB": "512"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("정했는데도 경고가 남습니다: %v", p.Warnings)
	}
	if p.Volume != "dbstudio-t1-data" {
		t.Errorf("볼륨 이름 = %s", p.Volume)
	}
}

// 카탈로그의 모든 종류가 계획까지 만들어져야 한다.
//
// 항목을 하나 더할 때 필수 칸이나 헬스체크를 빠뜨리는 것을 여기서 잡는다.
func TestEveryRecipeBuilds(t *testing.T) {
	for _, r := range Catalog() {
		t.Run(r.Label, func(t *testing.T) {
			vals := map[string]string{}
			for _, f := range r.Fields {
				if !f.Required {
					continue
				}
				switch f.Kind {
				case KindPassword:
					vals[f.Key] = "Pw1234!aB"
				case KindNumber:
					vals[f.Key] = "1"
				default:
					vals[f.Key] = f.Default
					if vals[f.Key] == "" {
						vals[f.Key] = "x"
					}
				}
			}
			p, err := Build(Spec{Kind: string(r.Kind), Name: "t1", Values: vals})
			if err != nil {
				t.Fatalf("필수 칸만 채웠는데 실패합니다: %v", err)
			}
			if p.Port == 0 {
				t.Error("포트가 없습니다")
			}
			if len(r.Health) == 0 {
				t.Error("헬스체크가 없습니다 — 뜨는 중과 고장을 구별할 수 없습니다")
			}
			if r.DataPath == "" {
				t.Error("데이터 경로가 없습니다")
			}
			req := p.CreateRequest("i")
			if _, ok := req.ExposedPorts[strconv.Itoa(p.Port)+"/tcp"]; !ok {
				t.Errorf("포트를 열지 않았습니다: %v", req.ExposedPorts)
			}
		})
	}
}

// 계획의 라벨이 도커 요청과 같아야 한다(목록에서 우리 것을 찾는 근거다).
func TestLabelsMatchRequest(t *testing.T) {
	p, _ := Build(spec("redis", nil))
	want := p.Labels("i9")
	got := p.CreateRequest("i9").Labels
	for k, v := range want {
		if got[k] != v {
			t.Errorf("라벨 %s = %q, 기대 %q", k, got[k], v)
		}
	}
}

// containsSeq는 args 안에 want 가 **붙어서** 들어 있는지 본다.
//
// 붙어 있는지를 보는 것이 요점이다. `-c` 와 `max_connections=200` 이 각각
// 있기만 하고 떨어져 있으면 PostgreSQL 은 `-c` 의 값으로 엉뚱한 것을 읽는다.
func containsSeq(args, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for i := 0; i+len(want) <= len(args); i++ {
		ok := true
		for j, w := range want {
			if args[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

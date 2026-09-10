package provision

import (
	"strings"
	"testing"
)

func planFor(t *testing.T, kind string, extra map[string]string) *Plan {
	t.Helper()
	p, err := Build(spec(kind, extra))
	if err != nil {
		t.Fatalf("%s 계획: %v", kind, err)
	}
	return p
}

// 비밀번호가 파일에 실려서는 안 된다.
//
// 이 파일은 저장소에 들어가라고 만드는 것이다. 평문을 적으면 그것이 새는 가장
// 흔한 길이 된다 — 그리고 한 번 커밋되면 되돌려도 이력에 남는다.
func TestComposeNeverWritesSecrets(t *testing.T) {
	for _, kind := range []string{"postgres", "mysql", "mariadb", "mongodb", "redis", "clickhouse", "mssql"} {
		p := planFor(t, kind, map[string]string{"password": "S3cret-도망간다"})
		f := Compose([]ComposeItem{{Service: "db1", Plan: p}})
		if strings.Contains(f.YAML, "S3cret-도망간다") {
			t.Errorf("%s: 비밀번호가 compose.yml 에 실렸습니다", kind)
		}
		if strings.Contains(f.EnvExample, "S3cret-도망간다") {
			t.Errorf("%s: 비밀번호가 .env 뼈대에 실렸습니다", kind)
		}
		// 자리는 있어야 한다. 없으면 그 DB 는 비밀번호 없이 뜨려다 실패한다.
		if !strings.Contains(f.YAML, "${DB1_") {
			t.Errorf("%s: 비밀 자리가 없습니다:\n%s", kind, f.YAML)
		}
		if !strings.HasSuffix(strings.TrimSpace(f.EnvExample), "=") {
			t.Errorf("%s: .env 뼈대에 값이 들어 있습니다:\n%s", kind, f.EnvExample)
		}
	}
}

// 한 파일에 DB 가 여럿이면 변수 이름이 겹치면 안 된다.
//
// 겹치면 한 값이 두 DB 의 비밀번호가 된다. 그것은 오류 없이 동작하다가,
// 한쪽 비밀번호를 바꾸는 날 다른 쪽이 함께 잠긴다.
func TestComposeSecretNamesDoNotCollide(t *testing.T) {
	a := planFor(t, "postgres", nil)
	b := planFor(t, "postgres", nil)
	f := Compose([]ComposeItem{
		{Service: "pg-one", Plan: a},
		{Service: "pg-two", Plan: b},
	})
	// 이름은 **필드 열쇠**에서 나온다(POSTGRES_PASSWORD 가 아니라 PASSWORD).
	// 비밀이 환경변수로만 가지 않기 때문이다 — Redis 는 인자로 가고, 그때는
	// 붙일 환경변수 이름 자체가 없다.
	if !strings.Contains(f.EnvExample, "PG_ONE_PASSWORD=") ||
		!strings.Contains(f.EnvExample, "PG_TWO_PASSWORD=") {
		t.Errorf("변수 이름이 갈라지지 않았습니다:\n%s", f.EnvExample)
	}
}

// 헬스체크의 $ 는 컨테이너 안 셸의 것이다.
//
// compose 는 파일을 읽으며 $VAR 를 자기가 채워 넣는다. 그대로 두면 빈 값으로
// 바뀌고, 헬스체크는 계정 없이 돌다 영영 실패한다 — 그러면 DB 는 잘 떠 있는데
// compose 는 영원히 "starting" 이라고 말한다.
func TestComposeEscapesDollarsInHealthcheck(t *testing.T) {
	p := planFor(t, "postgres", nil)
	f := Compose([]ComposeItem{{Service: "pg1", Plan: p}})

	if !strings.Contains(f.YAML, "$${POSTGRES_USER}") {
		t.Errorf("$ 가 두 번 적히지 않았습니다:\n%s", f.YAML)
	}
	// 우리가 넣은 자리(${PG1_...})는 compose 가 채워야 하므로 한 번이어야 한다.
	if strings.Contains(f.YAML, "$${PG1_") {
		t.Errorf("비밀 자리의 $ 까지 두 번 적혔습니다:\n%s", f.YAML)
	}
}

// 볼륨과 네트워크에는 name 을 적어야 한다.
//
// 적지 않으면 compose 가 프로젝트 이름을 앞에 붙여 **새 볼륨**을 만든다.
// 같은 기계에서 돌릴 때 쓰던 데이터가 아니라 빈 데이터로 뜨고, 오류는 나지 않는다.
func TestComposePinsVolumeAndNetworkNames(t *testing.T) {
	p := planFor(t, "postgres", map[string]string{"persist": "true"})
	f := Compose([]ComposeItem{{Service: "pg1", Plan: p}})

	if p.Volume == "" {
		t.Fatal("볼륨이 없는 계획입니다")
	}
	if !strings.Contains(f.YAML, "  "+p.Volume+":\n    name: \""+p.Volume+"\"") {
		t.Errorf("볼륨 이름이 고정되지 않았습니다:\n%s", f.YAML)
	}
	if !strings.Contains(f.YAML, "  "+NetworkName+":\n    name: \""+NetworkName+"\"") {
		t.Errorf("네트워크 이름이 고정되지 않았습니다:\n%s", f.YAML)
	}
}

// 설정 파일은 옆에 두고 **파일 하나만** 마운트해야 한다.
//
// 디렉터리째 덮으면 이미지가 넣어 둔 것이 사라진다 — ClickHouse 에서는
// listen_host 설정과 엔트리포인트가 만드는 비밀번호 파일이 그렇게 사라지고,
// 서버는 붙을 수 없거나 비밀번호 없이 뜬다.
func TestComposeMountsConfigFilesOneByOne(t *testing.T) {
	p := planFor(t, "clickhouse", map[string]string{"markCacheMB": "256", "maxQueryMemoryMB": "900"})
	f := Compose([]ComposeItem{{Service: "ch1", Plan: p}})

	if len(f.Files) != len(p.Files) || len(f.Files) == 0 {
		t.Fatalf("함께 저장할 파일이 %d개입니다 (계획은 %d개)", len(f.Files), len(p.Files))
	}
	for path := range p.Files {
		want := "./ch1" + path + ":" + path + ":ro"
		if !strings.Contains(f.YAML, want) {
			t.Errorf("파일 마운트가 없습니다: %s\n%s", want, f.YAML)
		}
		if !strings.Contains(f.YAML, "config.d/dbstudio.xml:ro") &&
			!strings.Contains(f.YAML, "users.d/dbstudio.xml:ro") {
			t.Errorf("파일이 아니라 디렉터리를 마운트한 것 같습니다:\n%s", f.YAML)
		}
	}
	if len(f.Notes) == 0 {
		t.Error("파일을 함께 두어야 한다는 안내가 없습니다")
	}
}

// 포트를 정하지 않았으면 번호를 지어내지 않는다.
//
// 아무 번호나 적으면 그 번호가 이미 쓰이고 있는 기계에서 뜨지 않는다.
func TestComposeLeavesRandomPortAlone(t *testing.T) {
	p := planFor(t, "redis", map[string]string{"hostPort": "0"})
	f := Compose([]ComposeItem{{Service: "rd1", Plan: p}})
	if !strings.Contains(f.YAML, "- \"6379\"\n") {
		t.Errorf("포트 자리가 잘못됐습니다:\n%s", f.YAML)
	}

	// 실제로 잡힌 포트를 주면 그것을 쓴다.
	f = Compose([]ComposeItem{{Service: "rd1", Plan: p, HostPort: 32801}})
	if !strings.Contains(f.YAML, "- \"32801:6379\"\n") {
		t.Errorf("잡힌 포트가 반영되지 않았습니다:\n%s", f.YAML)
	}
}

// 값은 언제나 따옴표로 감싼다.
//
// YAML 에서 따옴표 없는 `on`·`no`·`1.10` 은 문자열이 아닌 것으로 읽힌다.
// 무엇을 감싸야 하는지 고르려 들면 그 판단이 언젠가 틀린다.
func TestYamlStrAlwaysQuotes(t *testing.T) {
	cases := map[string]string{
		"on":             `"on"`,
		"1.10":           `"1.10"`,
		`따옴표"안`:          `"따옴표\"안"`,
		`역슬래시\길`:         `"역슬래시\\길"`,
		"줄\n바꿈":          `"줄\n바꿈"`,
		"unless-stopped": `"unless-stopped"`,
	}
	for in, want := range cases {
		if got := yamlStr(in); got != want {
			t.Errorf("%q → %s (기대 %s)", in, got, want)
		}
	}
}

// 같은 설정이면 같은 해시, 다르면 다른 해시.
//
// compose 가 "이 컨테이너가 지금 설정과 같은가"를 이 값으로 잰다. 매번 달라지면
// 다시 만들 때마다 "바뀌었다"고 보고, 늘 같으면 정말 바뀌었을 때 모른다.
func TestConfigHashFollowsTheSettings(t *testing.T) {
	a := planFor(t, "postgres", map[string]string{"maxConnections": "100"})
	b := planFor(t, "postgres", map[string]string{"maxConnections": "100"})
	c := planFor(t, "postgres", map[string]string{"maxConnections": "200"})

	if a.configHash() != b.configHash() {
		t.Error("같은 설정인데 해시가 다릅니다")
	}
	if a.configHash() == c.configHash() {
		t.Error("설정이 다른데 해시가 같습니다")
	}
	if len(a.configHash()) != 64 {
		t.Errorf("해시 길이 = %d", len(a.configHash()))
	}
}

// 라벨에 config-hash 가 있어야 compose 가 우리 컨테이너를 자기 것으로 본다.
//
// 살아 있는 데몬에 라벨을 하나씩 지워 가며 확인한 것이다. 이 라벨이 없으면
// 나머지를 다 갖춰도 `docker compose -p dbstudio ps` 에 나타나지 않는다.
func TestLabelsCarryComposeIdentity(t *testing.T) {
	p := planFor(t, "postgres", nil)
	l := p.Labels("dbi_1")

	for _, k := range []string{LabelProject, LabelService, LabelConfigHash, LabelOneOff, LabelContainer} {
		if l[k] == "" {
			t.Errorf("%s 라벨이 없습니다", k)
		}
	}
	// 딸린 파일 라벨은 **있어야 하되 비어 있어야** 한다. 없으면 docker compose ls 가
	// 컨테이너마다 경고를 한 줄씩 내고, 값을 지어내면 없는 파일을 가리키게 된다.
	if v, ok := l[LabelConfigFiles]; !ok || v != "" {
		t.Errorf("딸린 파일 라벨 = %q (있음: %v)", v, ok)
	}
	if l[LabelProject] != ProjectName {
		t.Errorf("프로젝트 = %s", l[LabelProject])
	}
	if l[LabelOneOff] != "False" {
		t.Errorf("oneoff = %s (compose 는 대문자 False 를 씁니다)", l[LabelOneOff])
	}
}

// 스택 파일은 docker stack deploy 가 거부하는 것을 넣지 않는다.
//
// 살아 있는 스웜에 돌려 보고 하나씩 찾은 것들이다. 이 검사가 없으면 다음에
// compose 쪽을 고치다 스택 쪽에 그것을 흘려보내게 된다.
func TestStackOmitsWhatSwarmRejects(t *testing.T) {
	p := planFor(t, "postgres", map[string]string{"persist": "true", "memoryMB": "512"})
	f := Stack([]ComposeItem{{Service: "pg1", Plan: p, HostPort: 32800}})

	if strings.Contains(f.YAML, "\nname:") {
		t.Errorf("최상위 name 이 있습니다 — stack deploy 가 거부합니다:\n%s", f.YAML)
	}
	if strings.Contains(f.YAML, "container_name:") {
		t.Errorf("container_name 이 있습니다 — 스웜이 무시하고 경고를 냅니다")
	}
	if strings.Contains(f.YAML, "\n    restart:") {
		t.Errorf("restart 가 있습니다 — deploy.restart_policy 로 적어야 합니다")
	}
	if !strings.Contains(f.YAML, "restart_policy:\n        condition: \"any\"") {
		t.Errorf("다시 시작 정책이 없습니다:\n%s", f.YAML)
	}
	// 네트워크는 오버레이여야 하고 이름을 고정하면 이미 있는 브리지와 부딪힌다.
	if !strings.Contains(f.YAML, "  "+NetworkName+":\n    driver: overlay\n") {
		t.Errorf("오버레이 네트워크가 아닙니다:\n%s", f.YAML)
	}
	if strings.Contains(f.YAML, "  "+NetworkName+":\n    name:") {
		t.Errorf("네트워크 이름을 고정했습니다 — already exists 로 멈춥니다")
	}
}

// 스택 파일의 비밀 자리는 **값이 없으면 배포가 멈추는** 모양이어야 한다.
//
// docker stack deploy 는 .env 를 읽지 않는다. 그냥 ${VAR} 로 두면 빈 값이 되고
// DB 가 비밀번호 없이 조용히 뜬다 — 실제로 그렇게 떴고, 붙어 보니 비밀번호 없이
// 들어갔다. 이 검사가 지키는 것이 그것이다.
func TestStackFailsLoudlyWithoutSecrets(t *testing.T) {
	p := planFor(t, "postgres", nil)
	f := Stack([]ComposeItem{{Service: "pg1", Plan: p}})

	if strings.Contains(f.YAML, "${PG1_PASSWORD}") {
		t.Errorf("값이 없어도 그냥 넘어가는 모양입니다:\n%s", f.YAML)
	}
	if !strings.Contains(f.YAML, "${PG1_PASSWORD:?") {
		t.Errorf("멈추게 하는 표시가 없습니다:\n%s", f.YAML)
	}
	// compose 쪽은 .env 를 읽으므로 그대로 둔다.
	c := Compose([]ComposeItem{{Service: "pg1", Plan: p}})
	if !strings.Contains(c.YAML, "${PG1_PASSWORD}") {
		t.Errorf("compose 쪽까지 멈추게 만들었습니다:\n%s", c.YAML)
	}
	// 안내에 .env 를 읽지 않는다는 말이 있어야 한다.
	joined := strings.Join(f.Notes, " ")
	if !strings.Contains(joined, ".env") {
		t.Errorf("안내에 .env 이야기가 없습니다: %v", f.Notes)
	}
}

// 두 파일은 **같은 것을 말해야** 한다.
//
// 한 렌더러가 둘을 만드는 이유다. 이미지·포트·환경변수 이름·설정 파일은
// 형식이 달라도 같아야 한다 — 다르면 화면에서 만든 것과 내보낸 것이 갈린다.
func TestComposeAndStackDescribeTheSameThing(t *testing.T) {
	p := planFor(t, "clickhouse", map[string]string{"markCacheMB": "256"})
	item := ComposeItem{Service: "ch1", Plan: p, HostPort: 32900}
	c, s := Compose([]ComposeItem{item}), Stack([]ComposeItem{item})

	for _, want := range []string{
		"image: \"" + p.Image + "\"", "32900:9000", "CLICKHOUSE_DB", "ulimits:",
		"./ch1/etc/clickhouse-server/config.d/dbstudio.xml",
	} {
		if !strings.Contains(c.YAML, want) {
			t.Errorf("compose 에 %q 가 없습니다", want)
		}
		if !strings.Contains(s.YAML, want) {
			t.Errorf("stack 에 %q 가 없습니다", want)
		}
	}
	if len(c.Files) != len(s.Files) {
		t.Errorf("함께 저장할 파일이 다릅니다: compose %d, stack %d", len(c.Files), len(s.Files))
	}
}

func TestSwarmRestartMapping(t *testing.T) {
	cases := map[string]string{
		"unless-stopped": "any", "always": "any",
		"on-failure": "on-failure", "no": "none", "": "none",
	}
	for in, want := range cases {
		if got := swarmRestart(in); got != want {
			t.Errorf("%q → %q (기대 %q)", in, got, want)
		}
	}
}

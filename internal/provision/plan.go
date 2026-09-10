package provision

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/docker"
)

// 설정값 → 컨테이너 계획.
//
// ── 왜 계획을 따로 만드는가 ─────────────────────────────────────────
// 화면에서 온 값을 바로 도커에 넘기지 않는다. 사이에 계획을 두면 셋이 가능해진다.
//
//  1. 만들기 전에 **무엇이 만들어지는지 보여줄 수 있다**. 컨테이너 이름, 이미지,
//     포트, 볼륨 — 누르기 전에 알 수 있어야 되돌릴 일이 줄어든다.
//  2. 같은 계획에서 compose.yml 을 쓸 수 있다(다음 단계). 두 길이 같은 값을
//     각자 해석하면 화면에서 만든 것과 내보낸 파일이 갈린다.
//  3. 검사할 수 있다. 도커에 붙지 않고도 "이 설정이면 이 환경변수가 간다"를
//     지킬 수 있다.

// 라벨.
//
// compose 와 같은 이름을 쓴다. 그래야 `docker compose ls` 와 `docker ps` 에서
// 우리가 만든 것이 하나의 프로젝트로 보인다 — 우리 라벨만 쓰면 도커 CLI 를
// 쓰는 사람에게는 정체 모를 컨테이너가 흩어져 있는 것으로 보인다.
const (
	LabelProject = "com.docker.compose.project"
	LabelService = "com.docker.compose.service"
	// LabelConfigHash가 **결정적이다.**
	//
	// project·service 라벨만으로는 compose 가 우리 컨테이너를 자기 프로젝트의
	// 것으로 보지 않는다. 살아 있는 데몬에 라벨을 하나씩 지워 가며 확인했다:
	// 이 라벨이 있으면 `docker compose -p dbstudio ps` 에 나타나고, 없으면
	// 나머지를 다 갖춰도 나타나지 않는다.
	LabelConfigHash = "com.docker.compose.config-hash"
	// 아래 셋은 compose 가 만든 것과 모양을 맞추는 값이다.
	//
	// oneoff 는 `docker compose run` 으로 잠깐 띄운 것과 구분하는 표시이고
	// (그것들은 목록에서 빠진다), container-number 는 같은 서비스의 몇 번째
	// 인스턴스인지다 — 우리는 서비스마다 하나뿐이라 늘 1 이다.
	// LabelNetwork는 네트워크에 붙는다. 없으면 `docker compose up` 이
	// **거절한다** — "network dbstudio-net was found but has incorrect label".
	// 내보낸 파일을 실제로 돌려 보고서야 알았다.
	LabelNetwork   = "com.docker.compose.network"
	LabelOneOff    = "com.docker.compose.oneoff"
	LabelContainer = "com.docker.compose.container-number"
	LabelVersion   = "com.docker.compose.version"
	// LabelConfigFiles는 **빈 값으로** 붙인다.
	//
	// 우리가 만든 컨테이너에는 딸린 파일이 없다(내보내기를 눌러야 파일이 생기고,
	// 그 파일이 어디 저장될지는 우리가 모른다). 그런데 이 라벨이 아예 없으면
	// `docker compose ls` 가 컨테이너마다 경고를 한 줄씩 낸다 — 우리가 남의
	// 도구를 시끄럽게 만드는 셈이다. 빈 값이면 경고 없이 "없음"으로 보인다.
	//
	// 없는 경로를 지어내지 않는 이유: 도커 데스크톱이 그 파일을 열려고 한다.
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelManagedBy   = "dbstudio.managed"
	LabelInstance    = "dbstudio.instance"
	LabelKind        = "dbstudio.kind"
	// ProjectName은 우리가 만든 것들이 모이는 compose 프로젝트 이름이다.
	ProjectName = "dbstudio"
	// NetworkName은 만든 DB 들이 붙는 네트워크다.
	//
	// 하나에 모으는 이유: 같은 도커 안의 다른 컨테이너(DB Studio 자신을 포함해)가
	// 이름으로 붙을 수 있어야 한다. 기본 브리지에서는 이름으로 못 찾는다.
	NetworkName = "dbstudio-net"
)

// Spec은 사람이 정한 것이다.
type Spec struct {
	// Kind는 카탈로그의 종류다.
	Kind string `json:"kind"`
	// Name은 이 인스턴스의 이름이다(컨테이너 이름의 바탕).
	Name string `json:"name"`
	// Version은 이미지 태그다. 비우면 레시피의 기본값.
	Version string `json:"version"`
	// Values는 필드 열쇠 → 값이다.
	Values map[string]string `json:"values"`
}

// Plan은 만들 것을 다 정한 상태다.
type Plan struct {
	Recipe *Recipe `json:"-"`

	// Container는 컨테이너 이름이다.
	Container string `json:"container"`
	Image     string `json:"image"`
	// Volume은 데이터 볼륨 이름이다(남기지 않으면 빈 문자열).
	Volume string `json:"volume,omitempty"`
	// HostPort는 호스트에 열 포트다. 0 이면 도커가 고른다.
	HostPort int `json:"hostPort"`
	// Port는 컨테이너 안 포트다.
	Port int `json:"port"`

	Env  map[string]string `json:"env"`
	Args []string          `json:"args,omitempty"`
	// Files는 시작하기 전에 컨테이너에 넣을 파일이다(경로 → 내용).
	Files map[string]string `json:"files,omitempty"`

	// Health는 이 계획만의 헬스체크다(비우면 레시피의 것을 쓴다).
	//
	// 왜 필요한가: 같은 이미지로 다른 것을 띄울 때가 있다. ClickHouse Keeper 는
	// clickhouse-server 이미지로 뜨지만 DB 가 아니라서 clickhouse-client 로
	// 물어볼 수 없다 — 레시피의 헬스체크를 그대로 쓰면 잘 도는 조정자가
	// 영원히 "뜨는 중"으로 남는다.
	Health []string `json:"-"`

	MemoryMB int    `json:"memoryMB,omitempty"`
	CPUs     string `json:"cpus,omitempty"`
	Restart  string `json:"restart"`

	// Secrets는 감사·미리보기에서 가려야 하는 환경변수 이름들이다.
	Secrets []string `json:"-"`
	// secretValues는 밖으로 나가면 안 되는 **값**이다(필드 열쇠 → 값).
	//
	// 이름이 아니라 값으로 들고 있는 이유: 비밀이 환경변수로만 가지 않는다.
	// Redis 의 비밀번호는 실행 인자로 간다(--requirepass). 환경변수 이름만
	// 가리면 compose 파일에 그 값이 인자로 실려 나간다 — 실제로 그렇게 새는
	// 것을 검사가 잡았다.
	//
	// 내보내지 않는 필드다(소문자). 계획 미리보기 응답에 실리지 않아야 한다.
	secretValues map[string]string

	// Warnings는 만들 수는 있지만 알아야 하는 것이다.
	Warnings []string `json:"warnings,omitempty"`
}

// Validate와 Build를 한 번에 한다.
//
// 갈라 두지 않는 이유: 검사에 통과한 값으로만 계획을 만들 수 있고, 계획을
// 만들면서 알게 되는 것(빈 포트를 못 잡았다)도 검사 결과다. 둘로 나누면
// 부르는 쪽이 순서를 지켜야 하고, 그 순서는 언젠가 어긋난다.
func Build(spec Spec) (*Plan, error) {
	r := Find(spec.Kind)
	if r == nil {
		if why := NotInCatalog(spec.Kind); why != "" {
			return nil, fmt.Errorf("%s 는 아직 만들 수 없습니다: %s", spec.Kind, why)
		}
		return nil, fmt.Errorf("%s 는 모르는 DB 종류입니다", spec.Kind)
	}

	name := strings.TrimSpace(spec.Name)
	if err := validName(name); err != nil {
		return nil, err
	}

	version := strings.TrimSpace(spec.Version)
	if version == "" {
		version = r.DefaultVersion()
	}
	// 태그를 목록으로 막는다. 임의의 문자열을 받으면 이미지 이름에 다른 것을
	// 끼워 넣을 수 있고(`17-alpine ; rm -rf`는 아니지만 다른 레지스트리는 된다),
	// 무엇보다 우리가 시험해 본 조합만 권하는 편이 정직하다.
	if !contains(r.Versions, version) {
		return nil, fmt.Errorf("%s 는 고를 수 없는 판본입니다 (%s)",
			version, strings.Join(r.Versions, ", "))
	}

	p := &Plan{
		Recipe:    r,
		Container: ContainerName(name),
		Image:     r.Image + ":" + version,
		Port:      r.Port,
		Env:       map[string]string{},
		Files:     map[string]string{},
		Restart:   "unless-stopped",
	}

	// 파일로 가는 값은 모아서 마지막에 한 파일로 만든다.
	fileVals := map[string]string{}
	p.secretValues = map[string]string{}

	for i := range r.Fields {
		f := &r.Fields[i]
		raw, given := spec.Values[f.Key]
		raw = strings.TrimSpace(raw)
		if !given || raw == "" {
			raw = f.Default
		}
		if f.Required && raw == "" {
			return nil, fmt.Errorf("%s 를 입력하세요", f.Label)
		}
		if raw == "" {
			continue // 안 정한 것은 이미지 기본값에 맡긴다
		}
		if err := checkField(f, raw); err != nil {
			return nil, err
		}
		// 어느 길로 가든 비밀이면 값을 적어 둔다. 길마다 따로 적으면 새 길을
		// 더할 때 한쪽을 빠뜨리고, 빠뜨린 쪽으로 값이 새어 나간다.
		if f.Secret {
			p.secretValues[f.Key] = raw
		}

		switch f.Target {
		case TargetEnv:
			p.Env[f.Name] = raw
			if f.Secret {
				p.Secrets = append(p.Secrets, f.Name)
			}
		case TargetArg:
			p.Args = append(p.Args, argFor(r, f, raw)...)
		case TargetFile:
			fileVals[f.Name] = fileValue(f, raw)
		case TargetMeta:
			if err := applyMeta(p, f, raw); err != nil {
				return nil, err
			}
		}
	}

	if len(fileVals) > 0 {
		for path, body := range renderConfigFiles(r, fileVals) {
			p.Files[path] = body
		}
	}

	if p.Volume == "" && r.DataPath != "" {
		p.Warnings = append(p.Warnings,
			"데이터를 남기지 않습니다. 컨테이너를 지우면 데이터도 함께 사라집니다")
	}
	if p.MemoryMB == 0 {
		p.Warnings = append(p.Warnings,
			"메모리 상한이 없습니다. 이 DB 가 기계의 메모리를 다 먹으면 DB Studio 자신도 함께 죽습니다")
	}
	// 인자는 정렬하지 않는다. `-c` 와 그 값이 짝이라 순서를 바꾸면 갈라진다.
	// 필드 순서가 곧 인자 순서이고, 그 순서는 레시피가 정한다.
	sort.Strings(p.Secrets)
	return p, nil
}

// ContainerName은 인스턴스 이름에서 컨테이너 이름을 만든다.
//
// 접두사를 붙이는 이유: `docker ps` 에서 우리가 만든 것을 한눈에 알아야 하고,
// 사람이 직접 만든 `postgres` 와 이름이 부딪히지 않아야 한다.
func ContainerName(name string) string { return "dbstudio-" + name }

// VolumeName은 데이터 볼륨 이름이다.
func VolumeName(name string) string { return "dbstudio-" + name + "-data" }

// NetworkLabels는 우리 네트워크에 붙일 라벨이다.
//
// compose 가 만든 것과 같은 모양이어야 한다. 우리가 먼저 만들어 둔 네트워크에
// 이것이 없으면, 내보낸 compose 파일로 띄우려 할 때 compose 가 "라벨이 다르다"며
// 멈춘다. 그 오류는 네트워크 이야기를 하지만 사람이 한 일은 DB 를 띄운 것뿐이라
// 무엇을 고쳐야 하는지 알기 어렵다.
func NetworkLabels() map[string]string {
	return map[string]string{
		LabelProject:   ProjectName,
		LabelNetwork:   NetworkName,
		LabelVersion:   "dbstudio",
		LabelManagedBy: "dbstudio",
	}
}

// healthTest는 이 계획의 헬스체크다(계획의 것이 있으면 그것, 없으면 레시피의 것).
func (p *Plan) healthTest() []string {
	if len(p.Health) > 0 {
		return p.Health
	}
	if p.Recipe != nil {
		return p.Recipe.Health
	}
	return nil
}

// Labels는 이 계획으로 만드는 것들에 붙일 라벨이다.
func (p *Plan) Labels(instanceID string) map[string]string {
	name := strings.TrimPrefix(p.Container, "dbstudio-")
	return map[string]string{
		LabelProject:     ProjectName,
		LabelService:     name,
		LabelConfigHash:  p.configHash(),
		LabelOneOff:      "False",
		LabelContainer:   "1",
		LabelConfigFiles: "",
		// 우리가 만들었다는 것을 버전 자리에 적는다. 숫자를 흉내 내면 compose 의
		// 어느 판이 만든 것인지 묻는 사람에게 거짓을 말하게 된다.
		LabelVersion:   "dbstudio",
		LabelManagedBy: "dbstudio",
		LabelInstance:  instanceID,
		LabelKind:      string(p.Recipe.Kind),
	}
}

// CreateRequest는 도커에 보낼 요청이다.
func (p *Plan) CreateRequest(instanceID string) *docker.CreateRequest {
	portKey := strconv.Itoa(p.Port) + "/tcp"

	env := make([]string, 0, len(p.Env))
	for _, k := range sortedKeys(p.Env) {
		env = append(env, k+"="+p.Env[k])
	}

	host := &docker.HostConfig{
		PortBindings: map[string][]docker.PortBinding{
			portKey: {{HostPort: strconv.Itoa(p.HostPort)}},
		},
		RestartPolicy: &docker.RestartPolicy{Name: p.Restart},
	}
	if p.Volume != "" {
		host.Mounts = []docker.Mount{
			{Type: "volume", Source: p.Volume, Target: p.Recipe.DataPath},
		}
	}
	if p.MemoryMB > 0 {
		host.Memory = int64(p.MemoryMB) << 20
	}
	if n := nanoCPUs(p.CPUs); n > 0 {
		host.NanoCPUs = n
	}
	// ClickHouse 는 파일 핸들 상한을 올려 줘야 뜨는 호스트가 있고, 그 오류
	// 메시지로는 원인을 짐작할 수 없다. 레시피가 아니라 여기서 다루는 이유는
	// 사람이 정할 값이 아니기 때문이다.
	if p.Recipe.ID == "clickhouse" {
		host.Ulimits = []docker.Ulimit{{Name: "nofile", Soft: 262144, Hard: 262144}}
	}

	req := &docker.CreateRequest{
		Image:        p.Image,
		Env:          env,
		Cmd:          p.Args,
		Labels:       p.Labels(instanceID),
		ExposedPorts: map[string]struct{}{portKey: {}},
		HostConfig:   host,
		NetworkingConfig: &docker.NetworkingConfig{
			EndpointsConfig: map[string]struct{}{NetworkName: {}},
		},
	}
	if health := p.healthTest(); len(health) > 0 {
		req.Healthcheck = &docker.HealthConfig{
			Test:     health,
			Interval: int64(5 * time.Second),
			Timeout:  int64(5 * time.Second),
			// 처음 뜰 때 데이터 디렉터리를 만드는 DB 가 있다(MySQL 은 수십 초).
			// 그동안의 실패를 세지 않아야 "뜨는 중"과 "고장"이 구별된다.
			StartPeriod: int64(30 * time.Second),
			Retries:     20,
		}
	}
	return req
}

// ---------- 값 해석 ----------

// applyMeta는 컨테이너에 넘기지 않는 값들을 계획에 담는다.
func applyMeta(p *Plan, f *Field, raw string) error {
	switch f.Name {
	case "hostPort":
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s 는 숫자여야 합니다", f.Label)
		}
		p.HostPort = n
	case "persist":
		if isTrue(raw) {
			p.Volume = VolumeName(strings.TrimPrefix(p.Container, "dbstudio-"))
		}
	case "memoryMB":
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s 는 숫자여야 합니다", f.Label)
		}
		p.MemoryMB = n
	case "cpus":
		if nanoCPUs(raw) < 0 {
			return fmt.Errorf("%s 는 1.5 처럼 적어 주세요", f.Label)
		}
		p.CPUs = raw
	case "restart":
		p.Restart = raw
	}
	return nil
}

// argFor는 실행 인자를 만든다. **낱개로** 돌려준다.
//
// 공백으로 이어 붙인 한 덩이를 돌려주면 안 된다. Cmd 는 argv 배열이라
// "-c max_connections=200" 한 덩이는 **인자 하나**로 전달되고, PostgreSQL 은
// 옵션 이름을 " max_connections"(앞에 공백)로 읽는다:
//
//	FATAL: unrecognized configuration parameter " max_connections"
//
// 처음에 그렇게 만들었고 실제 데몬에 돌려 본 검사가 잡았다. 셸을 거치지
// 않으므로 공백이 인자를 가르지 않는다 — 셸에서 시험하면 통과하는 종류의
// 착각이다.
//
// DB 마다 모양이 다르다. PostgreSQL 은 `-c` `이름=값` 둘, MySQL 계열은
// `--이름=값` 하나, Redis 는 `--이름` `값` 둘이다.
func argFor(r *Recipe, f *Field, raw string) []string {
	switch r.ID {
	case "postgres":
		return []string{"-c", f.Name + "=" + withUnit(f, raw, "MB")}
	case "redis":
		if f.Kind == KindBool {
			return []string{"--" + f.Name, yesNo(raw)}
		}
		return []string{"--" + f.Name, withUnit(f, raw, "mb")}
	default: // mysql·mariadb
		return []string{"--" + f.Name + "=" + withUnit(f, raw, "M")}
	}
}

// withUnit은 크기 값에 단위를 붙인다(숫자만 온 경우).
//
// 왜 필요한가: 화면은 "MB" 라고 물었으므로 사람은 숫자만 적는다. 그런데
// mysql 의 innodb-buffer-pool-size 는 단위 없는 숫자를 **바이트**로 읽는다 —
// 512 라고 적으면 512바이트가 되고, 그러면 MySQL 이 뜨지 않는다.
func withUnit(f *Field, raw, unit string) string {
	if !strings.HasSuffix(f.Key, "MB") && !strings.HasSuffix(f.Key, "GB") {
		return raw
	}
	if _, err := strconv.Atoi(raw); err != nil {
		return raw // 이미 단위가 붙어 있다
	}
	return raw + unit
}

// fileValue는 파일로 가는 값을 그 파일이 기대하는 단위로 바꾼다.
//
// MB 로 물어보고 바이트로 적는다. ClickHouse 의 설정은 단위 없는 숫자를
// 바이트로 읽으므로, 화면에서 적은 900 을 그대로 넘기면 900**바이트**가 된다 —
// 오류도 경고도 없고, 상한만 우습게 작아져서 모든 질의가 실패한다.
//
// 판단 기준을 키 이름(MB 로 끝나는가)에 두는 것은 인자 쪽의 withUnit 과 같다.
// 설정 이름(`_size` 로 끝나는가)으로 재면 max_memory_usage 처럼 이름에 단위가
// 없는 것이 조용히 빠진다 — 실제로 그렇게 빠져 있었다.
func fileValue(f *Field, raw string) string {
	shift := 0
	switch {
	case strings.HasSuffix(f.Key, "MB"):
		shift = 20
	case strings.HasSuffix(f.Key, "GB"):
		shift = 30
	default:
		return raw
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return raw // 사람이 단위를 직접 적었다
	}
	return strconv.Itoa(n << shift)
}

// renderConfigFiles은 파일로 가는 값들을 그 DB 의 설정 파일로 만든다.
//
// 지금은 ClickHouse 하나다. 그쪽은 **서버 설정과 프로필 설정이 다른 디렉터리에
// 있다** — config.d 와 users.d. 프로필 값을 config.d 에 적으면 오류도 경고도
// 없이 그냥 안 먹는다. 이 함수가 파일을 둘로 가르는 이유가 그것이다.
//
// 짐작으로 가른 것이 아니다. 처음에는 프로필 값도 config.d 에 <profiles> 로
// 적었는데, 살아 있는 서버에 물어보니 max_memory_usage 가 0 이었다
// (live_test.go 의 TestLiveClickHouseConfigTakesEffect). 그 검사가 이 갈라짐을
// 지킨다.
func renderConfigFiles(r *Recipe, vals map[string]string) map[string]string {
	if r.Kind != "clickhouse" {
		return nil
	}
	// 프로필 설정(질의 하나의 몫)과 서버 설정(캐시·전체 몫)을 가른다.
	profile := map[string]bool{"max_memory_usage": true, "max_threads": true}

	var server, prof []string
	for _, k := range sortedKeys(vals) {
		if profile[k] {
			prof = append(prof, k)
		} else {
			server = append(server, k)
		}
	}

	out := map[string]string{}
	if len(server) > 0 {
		var b strings.Builder
		b.WriteString("<!-- DB Studio 가 만든 서버 설정입니다. -->\n<clickhouse>\n")
		for _, k := range server {
			fmt.Fprintf(&b, "    <%s>%s</%s>\n", k, vals[k], k)
		}
		b.WriteString("</clickhouse>\n")
		out[ClickHouseServerConfig] = b.String()
	}
	if len(prof) > 0 {
		// users.d 다. 여기 적어야 default 프로필에 붙는다.
		var b strings.Builder
		b.WriteString("<!-- DB Studio 가 만든 프로필 설정입니다. -->\n")
		b.WriteString("<clickhouse>\n    <profiles>\n        <default>\n")
		for _, k := range prof {
			fmt.Fprintf(&b, "            <%s>%s</%s>\n", k, vals[k], k)
		}
		b.WriteString("        </default>\n    </profiles>\n</clickhouse>\n")
		out[ClickHouseUserConfig] = b.String()
	}
	return out
}

// ClickHouse 의 설정 파일 자리. 이미지가 만들어 둔 디렉터리라 우리가 만들지
// 않는다 — PutFile 은 없는 디렉터리를 만들지 않으므로 오타는 바로 걸린다.
//
// 디렉터리째로 마운트하지 않는 이유: config.d 에는 이미지가 넣어 둔
// docker_related_config.xml(listen_host 0.0.0.0)이 있고, users.d 에는
// 엔트리포인트가 만드는 default-user.xml(비밀번호)이 있다. 디렉터리를 덮으면
// 둘 다 사라지고, 서버는 붙을 수 없거나 비밀번호 없이 뜬다.
const (
	ClickHouseServerConfig = "/etc/clickhouse-server/config.d/dbstudio.xml"
	ClickHouseUserConfig   = "/etc/clickhouse-server/users.d/dbstudio.xml"
)

// ---------- 검사 ----------

// validName은 인스턴스 이름을 검사한다.
//
// 좁게 막는다. 이 이름이 컨테이너 이름과 볼륨 이름이 되고, 도커가 받는 글자가
// 우리보다 넓다 — 넓은 쪽에 맞추면 `../` 가 든 이름으로 볼륨을 만들 수 있다.
func validName(name string) error {
	if name == "" {
		return fmt.Errorf("이름을 입력하세요")
	}
	if len(name) > 40 {
		return fmt.Errorf("이름이 너무 깁니다 (40자 제한)")
	}
	for i, ch := range name {
		ok := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-'
		if i == 0 {
			ok = ch >= 'a' && ch <= 'z'
		}
		if !ok {
			return fmt.Errorf("이름은 소문자로 시작하고 소문자·숫자·붙임표(-)만 쓸 수 있습니다")
		}
	}
	return nil
}

func checkField(f *Field, raw string) error {
	switch f.Kind {
	case KindNumber:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%s 는 숫자여야 합니다", f.Label)
		}
		// Min 이 0 인 필드는 "0 은 제한 없음"이라는 뜻이라 그 값을 허용한다.
		if f.Max > 0 && n > f.Max {
			return fmt.Errorf("%s 는 %d 이하여야 합니다", f.Label, f.Max)
		}
		if f.Min > 0 && n != 0 && n < f.Min {
			return fmt.Errorf("%s 는 %d 이상이어야 합니다", f.Label, f.Min)
		}
	case KindSelect:
		if len(f.Choices) > 0 && !contains(f.Choices, raw) {
			return fmt.Errorf("%s 는 %s 중 하나여야 합니다", f.Label, strings.Join(f.Choices, ", "))
		}
	case KindBool:
		if !isTrue(raw) && !isFalse(raw) {
			return fmt.Errorf("%s 는 참/거짓이어야 합니다", f.Label)
		}
	}
	// 환경변수·인자로 가는 값에 줄바꿈이 있으면 안 된다. 인자 하나가 둘이 되고,
	// 환경변수는 그 자리에서 끊긴다.
	if strings.ContainsAny(raw, "\n\r\x00") {
		return fmt.Errorf("%s 에 줄바꿈을 쓸 수 없습니다", f.Label)
	}
	return nil
}

// ---------- 유틸 ----------

// nanoCPUs는 "1.5" 를 도커의 단위로 바꾼다. 잘못된 값이면 -1.
func nanoCPUs(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		return -1
	}
	return int64(v * 1e9)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no", "off", "":
		return true
	}
	return false
}

func yesNo(v string) string {
	if isTrue(v) {
		return "yes"
	}
	return "no"
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
	LabelProject   = "com.docker.compose.project"
	LabelService   = "com.docker.compose.service"
	LabelManagedBy = "dbstudio.managed"
	LabelInstance  = "dbstudio.instance"
	LabelKind      = "dbstudio.kind"
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

	MemoryMB int    `json:"memoryMB,omitempty"`
	CPUs     string `json:"cpus,omitempty"`
	Restart  string `json:"restart"`

	// Secrets는 감사·미리보기에서 가려야 하는 환경변수 이름들이다.
	Secrets []string `json:"-"`

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

		switch f.Target {
		case TargetEnv:
			p.Env[f.Name] = raw
			if f.Secret {
				p.Secrets = append(p.Secrets, f.Name)
			}
		case TargetArg:
			p.Args = append(p.Args, argFor(r, f, raw)...)
		case TargetFile:
			fileVals[f.Name] = raw
		case TargetMeta:
			if err := applyMeta(p, f, raw); err != nil {
				return nil, err
			}
		}
	}

	if len(fileVals) > 0 {
		path, body := renderConfigFile(r, fileVals)
		if path != "" {
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

// Labels는 이 계획으로 만드는 것들에 붙일 라벨이다.
func (p *Plan) Labels(instanceID string) map[string]string {
	name := strings.TrimPrefix(p.Container, "dbstudio-")
	return map[string]string{
		LabelProject:   ProjectName,
		LabelService:   name,
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
	if len(p.Recipe.Health) > 0 {
		req.Healthcheck = &docker.HealthConfig{
			Test:     p.Recipe.Health,
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

// renderConfigFile은 파일로 가는 값들을 그 DB 의 설정 파일로 만든다.
//
// 지금은 ClickHouse 하나다. 그쪽은 서버 설정과 프로필 설정이 다른 디렉터리에
// 있어서(config.d 와 users.d), 어느 값이 어디로 가는지를 알아야 한다 —
// 프로필 값을 config.d 에 적으면 오류도 없이 그냥 안 먹는다.
func renderConfigFile(r *Recipe, vals map[string]string) (string, string) {
	if r.Kind != "clickhouse" {
		return "", ""
	}
	// 프로필 설정(질의 하나의 몫)과 서버 설정(캐시·전체 몫)을 가른다.
	profile := map[string]bool{"max_memory_usage": true, "max_threads": true}

	var server, prof []string
	for _, k := range sortedKeys(vals) {
		v := vals[k]
		if strings.HasSuffix(k, "_size") || k == "mark_cache_size" {
			// MB 로 받아 바이트로 적는다. ClickHouse 는 단위 없는 숫자를
			// 바이트로 읽는다.
			if n, err := strconv.Atoi(v); err == nil {
				v = strconv.Itoa(n << 20)
			}
		}
		line := fmt.Sprintf("    <%s>%s</%s>", k, v, k)
		if profile[k] {
			prof = append(prof, line)
		} else {
			server = append(server, line)
		}
	}

	var b strings.Builder
	b.WriteString("<!-- DB Studio 가 만든 설정입니다. -->\n<clickhouse>\n")
	b.WriteString(strings.Join(server, "\n"))
	if len(prof) > 0 {
		b.WriteString("\n    <profiles>\n        <default>\n")
		for _, l := range prof {
			b.WriteString("    " + strings.TrimPrefix(l, "    ") + "\n")
		}
		b.WriteString("        </default>\n    </profiles>")
	}
	b.WriteString("\n</clickhouse>\n")
	return "/etc/clickhouse-server/config.d/dbstudio.xml", b.String()
}

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

package application

import (
	"encoding/json"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"postra/internal/platform/config"
	"postra/internal/platform/notifymail"
	"postra/internal/platform/tracking"
)

type SettingDefinition struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Category    string   `json:"category"`
	Type        string   `json:"type"`
	Default     string   `json:"default"`
	Environment []string `json:"environment,omitempty"`
	Scope       string   `json:"scope"`
	Apply       string   `json:"apply"`
	Secret      bool     `json:"secret"`
	Lockable    bool     `json:"lockable"`
	Options     []string `json:"options,omitempty"`
	Help        string   `json:"help,omitempty"`
}

// #nosec G101 -- UI display labels and setting identifiers, never credential values.
var settingLabels = map[string]string{
	"auth.session_hours":                  "세션 유지 시간(시간)",
	"auth.enabled":                        "사용자 인증 활성화",
	"auth.oidc.issuer":                    "Keycloak Issuer URL",
	"auth.oidc.client_id":                 "OIDC Client ID",
	"auth.oidc.secret_ref":                "OIDC Client Secret",
	"auth.oidc.redirect_url":              "OIDC Callback URL",
	"auth.oidc.auto_login":                "SSO 자동 로그인",
	"auth.oidc.auto_provision":            "SSO 사용자 자동 생성",
	"auth.oidc.admin_group":               "SSO 관리자 그룹",
	"ai.base_url":                         "OpenAI 호환 Base URL",
	"ai.model":                            "기본 Chat Model",
	"ai.embed_model":                      "Embedding Model",
	"ai.embed_base_url":                   "임베딩 전용 Base URL",
	"ai.api_key_ref":                      "AI API Key",
	"ai.timeout_sec":                      "AI Timeout(초)",
	"ai.max_tokens":                       "최대 출력 토큰",
	"ai.context_length":                   "Context Length 수동·대체 한도",
	"ai.auto_context_length":              "모델 Context Length 자동 감지",
	"ai.temperature":                      "기본 Temperature",
	"ai.disabled_models":                  "비활성 모델 목록",
	"ai.allow_external":                   "외부 AI 호출 허용",
	"ai.mask_external_pii":                "외부 AI 요청 PII 마스킹",
	"ai.stream":                           "AI 스트리밍",
	"ai.task_models":                      "작업별 모델 라우팅",
	"ai.extra_headers":                    "추가 API 헤더 (쓰기 전용 JSON)",
	"ai.extra_headers_ref":                "추가 API 헤더",
	"ai.cost_per_1m_input_tokens":         "입력 백만 토큰 비용",
	"ai.cost_per_1m_output_tokens":        "출력 백만 토큰 비용",
	"sync.auto_sync_minutes":              "기본 자동 동기화 간격(분)",
	"sync.initial_window_days":            "첫 수집 기간(일)",
	"sync.max_message_bytes":              "메일 한 건 최대 크기(bytes)",
	"sync.max_per_sync":                   "한 번에 수집할 최대 메일 수",
	"sync.connect_timeout_sec":            "연결 Timeout(초)",
	"sync.command_timeout_sec":            "메일 명령 Timeout(초)",
	"sync.auto_embed_minutes":             "자동 임베딩 간격(분)",
	"sync.auto_triage":                    "자동 AI 분류 활성화",
	"sync.auto_triage_minutes":            "자동 분류 간격(분)",
	"sync.daily_digest_enabled":           "일별 브리핑 활성화",
	"sync.daily_digest_hour":              "브리핑 생성 시각(서버 시간)",
	"sync.max_concurrent_syncs":           "최대 동시 동기화 수",
	"send.max_per_minute":                 "계정별 분당 발송 제한",
	"send.max_per_hour":                   "계정별 시간당 발송 제한",
	"send.warn_recipients":                "수신자 수 경고 기준",
	"send.max_retries":                    "발송 최대 시도 수",
	"send.retry_base_seconds":             "재시도 기본 간격(초)",
	"send.retry_max_seconds":              "재시도 최대 간격(초)",
	"send.dlp_policy":                     "민감정보 발송 정책",
	"send.dlp_keywords":                   "민감정보 키워드",
	"attachments.block_extensions":        "차단할 첨부 확장자",
	"attachments.quarantine_extensions":   "격리할 첨부 확장자",
	"attachments.archive_max_entries":     "압축파일 최대 항목 수",
	"attachments.archive_max_total_bytes": "압축 해제 최대 크기(bytes)",
	"attachments.archive_max_ratio":       "최대 압축률",
	"security.allow_insecure_mail":        "격리망 평문·무인증 메일 허용",
	"security.allow_private_hosts":        "사설망 메일 호스트 허용",
	"security.encrypt_at_rest":            "메일 원문 저장 암호화",
	"system.http_addr":                    "HTTP Listen 주소",
	"system.mcp_http_addr":                "별도 MCP Listen 주소",
	"system.metrics_enabled":              "Prometheus 활성화",
	"system.web_ui_enabled":               "공식 Web UI 활성화",
	"system.worker_enabled":               "백그라운드 Worker 활성화",
	"system.telemetry_enabled":            "OpenTelemetry 활성화",
	"storage.data_dir":                    "데이터 디렉터리",
	"storage.driver":                      "데이터베이스 종류",
	"storage.postgres_dsn":                "PostgreSQL 연결 정보",
	"security.api_token":                  "배포 API Token",
	"auth.bootstrap_admin":                "최초 관리자 ID",
	"auth.bootstrap_password":             "최초 관리자 비밀번호",
	"auth.oidc.client_secret":             "OIDC 환경변수 Client Secret",
	"compose.writing_guide":               "조직 메일 작성 지침",
	"compose.banned_phrases":              "작성 시 경고할 표현",
}

var settingCatalogOnce sync.Once
var settingCatalog []SettingDefinition

func SettingsDefinitions() []SettingDefinition {
	settingCatalogOnce.Do(func() { settingCatalog = buildSettingsDefinitions() })
	return settingCatalog
}

func buildSettingsDefinitions() []SettingDefinition {
	base := config.Default()
	defaults := config.BoundValues(base)
	result := make([]SettingDefinition, 0, len(config.Bindings)+80)
	for _, b := range config.Bindings {
		category := strings.Split(b.Key, ".")[0]
		if category == "compose" {
			category = "mail"
		}
		typ := "string"
		value := reflect.ValueOf(base)
		for _, part := range strings.Split(b.Path, ".") {
			value = value.FieldByName(part)
		}
		switch value.Kind() {
		case reflect.Bool:
			typ = "bool"
		case reflect.Int, reflect.Int64:
			typ = "int"
		case reflect.Float64:
			typ = "number"
		case reflect.Map:
			typ = "json"
		case reflect.Slice:
			typ = "string"
		}
		d := SettingDefinition{Key: b.Key, Label: settingLabels[b.Key], Category: category, Type: typ, Default: defaults[b.Key], Environment: []string{b.Env}, Scope: "admin", Apply: "live"}
		if d.Label == "" {
			d.Label = d.Key
		}
		if d.Key == "ai.extra_headers_ref" {
			d.Environment = append(d.Environment, "POSTRA_AI_EXTRA_HEADERS")
		}
		switch b.Key {
		case "system.http_addr", "system.mcp_http_addr", "system.metrics_enabled", "system.web_ui_enabled", "system.worker_enabled", "system.telemetry_enabled", "auth.enabled":
			d.Apply = "restart"
		case "storage.data_dir", "storage.driver", "storage.postgres_dsn", "security.api_token", "auth.bootstrap_admin", "auth.bootstrap_password", "auth.oidc.client_secret", "security.encrypt_at_rest":
			d.Apply = "deployment"
			d.Help = "새 프로세스나 저장소를 여는 데 필요한 배포 초기값입니다. 운영 중 안전하게 바꿀 수 없으므로 배포 환경에서 변경하세요."
		}
		switch b.Key {
		case "storage.postgres_dsn", "security.api_token", "auth.bootstrap_password", "auth.oidc.client_secret", "auth.oidc.secret_ref", "ai.api_key_ref", "ai.extra_headers", "ai.extra_headers_ref":
			d.Secret = true
			d.Default = ""
			d.Type = "secret"
		}
		if strings.HasSuffix(b.Key, "base_url") || b.Key == "auth.oidc.issuer" || b.Key == "auth.oidc.redirect_url" {
			d.Type = "url"
		}
		if b.Key == "send.dlp_policy" {
			d.Type = "enum"
			d.Options = []string{"off", "warn", "block"}
		}
		switch b.Key {
		case "ai.auto_context_length":
			d.Help = "선택한 모델과 Endpoint의 /models에서 실제 Context 한도를 자동 조회합니다. 한도를 제공하지 않거나 조회할 수 없으면 수동·대체 한도를 사용합니다. 저장 즉시 다음 요청부터 반영됩니다."
		case "ai.context_length":
			d.Help = "자동 감지를 끄거나 서버가 한도를 제공하지 않을 때 사용할 입력·출력 합계 한도입니다. 자동 감지가 성공하면 감지된 값이 우선합니다. 입력 본문은 임의로 자르지 않습니다."
		case "ai.max_tokens":
			d.Help = "요청할 최대 출력 토큰입니다. 모델·작업별 출력 한도와 입력 후 남은 Context에 맞춰 더 작게 조정될 수 있습니다."
		}
		result = append(result, d)
	}
	extra := []SettingDefinition{
		{Key: "general.product_name", Label: "제품 이름", Category: "general", Type: "string", Default: "Postra", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "general.default_language", Label: "기본 언어", Category: "general", Type: "enum", Default: "ko", Scope: "admin", Apply: "live", Lockable: false, Options: []string{"ko", "en"}},
		{Key: "mail.imap_enabled", Label: "IMAP 연결 허용", Category: "mail", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mail.pop3_enabled", Label: "POP3 연결 허용", Category: "mail", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mail.smtp_enabled", Label: "SMTP 발송 허용", Category: "mail", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mail.tls_required", Label: "메일 TLS 필수", Category: "mail", Type: "bool", Default: "false", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mail.smtp_auth_required", Label: "SMTP 인증 필수", Category: "mail", Type: "bool", Default: "false", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mail.html_enabled", Label: "HTML 메일 허용", Category: "mail", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mail.external_images", Label: "수신 외부 이미지 정책", Category: "security", Type: "enum", Default: "block", Scope: "admin", Apply: "live", Lockable: false, Options: []string{"block", "allow_once", "allow_sender", "allow_domain"}},
		{Key: "mail.outbound_images", Label: "발송 외부 이미지 정책", Category: "security", Type: "enum", Default: "block", Scope: "admin", Apply: "live", Lockable: false, Options: []string{"block", "allow", "proxy"}},
		{Key: "mail.image_proxy_url", Label: "외부 이미지 프록시 URL", Category: "security", Type: "url", Default: "", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "sync.min_sync_minutes", Label: "최소 자동 동기화 간격(분)", Category: "sync", Type: "int", Default: "1", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "sync.triage_mode", Label: "자동 AI 분류 대상", Category: "sync", Type: "enum", Default: "off", Scope: "admin", Apply: "live", Lockable: false, Options: []string{"off", "important_only", "rules", "all"}},
		{Key: "send.max_recipients", Label: "최대 수신자 수", Category: "send", Type: "int", Default: "100", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "send.warn_external", Label: "외부 수신자 경고", Category: "send", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "send.external_approval", Label: "외부 메일 승인 필수", Category: "send", Type: "bool", Default: "true", Scope: "admin", Apply: "fixed", Help: "모든 메일 발송에는 사용자의 명시적 승인이 항상 필요합니다. 관리자 설정이나 배포 환경에서도 해제할 수 없습니다."},
		{Key: "attachments.max_bytes", Label: "첨부파일 최대 크기(bytes)", Category: "attachments", Type: "int", Default: "26214400", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "attachments.max_count", Label: "최대 첨부파일 수", Category: "attachments", Type: "int", Default: "20", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "attachments.allow_extensions", Label: "허용할 첨부 확장자 (빈 값: 차단 목록 이외 허용)", Category: "attachments", Type: "string", Default: "", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "notifications.enabled", Label: "알림 활성화", Category: "notifications", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "notifications.poll_seconds", Label: "알림 갱신 간격(초)", Category: "notifications", Type: "int", Default: "15", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.enabled", Label: "MCP 활성화", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.http_enabled", Label: "HTTP MCP 활성화", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.endpoint", Label: "MCP Endpoint", Category: "mcp", Type: "string", Default: "/mcp", Scope: "admin", Apply: "restart", Lockable: false},
		{Key: "mcp.request_timeout_sec", Label: "MCP 요청 제한 시간(초)", Category: "mcp", Type: "int", Default: "120", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.session_timeout_sec", Label: "MCP 세션 제한 시간(초)", Category: "mcp", Type: "int", Default: "1800", Scope: "admin", Apply: "restart", Help: "진행 중인 발송과 MCP 세션을 보호하기 위해 재시작 후 적용합니다."},
		{Key: "vector.provider", Label: "벡터 저장소", Category: "search", Type: "enum", Default: "", Scope: "admin", Apply: "live", Lockable: false, Options: []string{"", "sqlite", "postgres", "milvus"}},
		{Key: "vector.milvus_url", Label: "Milvus URL", Category: "search", Type: "url", Default: "", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "vector.milvus_token_ref", Label: "Milvus Token", Category: "search", Type: "secret", Default: "", Scope: "admin", Apply: "live", Lockable: false, Secret: true},
		{Key: "vector.milvus_collection", Label: "Milvus Collection", Category: "search", Type: "string", Default: "postra_emails", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.policy", Label: "MCP 역할·도구 정책", Category: "mcp", Type: "json", Default: "", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "ui.theme", Label: "테마", Category: "appearance", Type: "enum", Default: "system", Scope: "user", Apply: "live", Lockable: true, Options: []string{"system", "light", "dark"}},
		{Key: "ui.density", Label: "화면 밀도", Category: "appearance", Type: "enum", Default: "comfortable", Scope: "user", Apply: "live", Lockable: true, Options: []string{"comfortable", "compact"}},
		{Key: "ui.reader_position", Label: "읽기 패널 위치", Category: "appearance", Type: "enum", Default: "right", Scope: "user", Apply: "live", Lockable: true, Options: []string{"right", "bottom", "hidden"}},
		{Key: "ui.preview_lines", Label: "목록 미리보기 줄 수", Category: "appearance", Type: "int", Default: "2", Scope: "user", Apply: "live", Lockable: true},
		{Key: "ui.ai_panel", Label: "AI 패널 기본 표시", Category: "appearance", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "mail.default_account_id", Label: "기본 메일 계정", Category: "personal_mail", Type: "account", Default: "", Scope: "user", Apply: "live"},
		{Key: "compose.signature_id", Label: "기본 서명", Category: "compose", Type: "signature", Default: "", Scope: "user", Apply: "live"},
		{Key: "compose.use_signature", Label: "서명 자동 적용", Category: "compose", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "compose.format", Label: "메일 작성 기본 형식", Category: "compose", Type: "enum", Default: "auto", Scope: "user", Apply: "live", Lockable: true, Options: []string{"auto", "text", "markdown", "html"}},
		{Key: "compose.template", Label: "기본 HTML 템플릿", Category: "compose", Type: "enum", Default: "clean", Scope: "user", Apply: "live", Lockable: true, Options: []string{"clean", "formal", "concise", "notice", "report", "newsletter", "plain"}},
		{Key: "compose.tone", Label: "AI 작성 톤", Category: "compose", Type: "enum", Default: "professional", Scope: "user", Apply: "live", Lockable: true, Options: []string{"professional", "formal", "friendly", "concise", "apologetic", "persuasive", "executive"}},
		{Key: "compose.reply_length", Label: "AI 답장 길이", Category: "compose", Type: "enum", Default: "short", Scope: "user", Apply: "live", Lockable: true, Options: []string{"short", "medium", "long"}},
		{Key: "compose.language", Label: "작성 언어", Category: "compose", Type: "enum", Default: "ko", Scope: "user", Apply: "live", Lockable: true, Options: []string{"ko", "en"}},
		{Key: "compose.writing_style", Label: "개인 작성 스타일", Category: "compose", Type: "text", Default: "", Scope: "user", Apply: "live", Lockable: true},
		{Key: "compose.signature_policy", Label: "서명 적용 정책", Category: "compose", Type: "enum", Default: "full", Scope: "user", Apply: "live", Lockable: true, Options: []string{"full", "smart", "none"}},
		{Key: "ai.auto_summary", Label: "메일 열 때 자동 요약", Category: "personal_ai", Type: "bool", Default: "false", Scope: "user", Apply: "live", Lockable: true},
		{Key: "ai.show_replies", Label: "AI 추천 답장 표시", Category: "personal_ai", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "search.default_mode", Label: "기본 검색 방식", Category: "personal_ai", Type: "enum", Default: "keyword", Scope: "user", Apply: "live", Lockable: true, Options: []string{"keyword", "semantic", "hybrid"}},
		{Key: "notifications.sync", Label: "동기화 알림", Category: "personal_notifications", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "notifications.send", Label: "발송 알림", Category: "personal_notifications", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "notifications.ai", Label: "AI 작업 알림", Category: "personal_notifications", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "notifications.action", Label: "업무·액션 알림", Category: "personal_notifications", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "notifications.security", Label: "보안 알림", Category: "personal_notifications", Type: "bool", Default: "true", Scope: "user", Apply: "live", Lockable: true},
		{Key: "ui.language", Label: "언어·날짜 로캘", Category: "appearance", Type: "enum", Default: "ko", Scope: "user", Apply: "live", Lockable: true, Options: []string{"ko", "en"}, Help: "HTML 언어 속성과 날짜 표시에 적용합니다. 운영 UI 문구는 한국어 중심이며 AI 작성 언어는 작성 설정에서 별도로 지정합니다."},
		{Key: "ui.date_format", Label: "날짜 표시", Category: "appearance", Type: "enum", Default: "relative", Scope: "user", Apply: "live", Lockable: true, Options: []string{"relative", "absolute", "iso"}},
		{Key: "account.auto_sync", Label: "계정 자동 동기화", Category: "account", Type: "bool", Default: "true", Scope: "account", Apply: "live", Lockable: false},
		{Key: "account.sync_minutes", Label: "계정 동기화 간격(분, 0은 조직 기본값)", Category: "account", Type: "int", Default: "0", Scope: "account", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.read", Label: "MCP read 권한", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.search", Label: "MCP search 권한", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.ai", Label: "MCP ai 권한", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.draft", Label: "MCP draft 권한", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.send", Label: "MCP send 권한", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.delete", Label: "MCP delete 권한", Category: "mcp", Type: "bool", Default: "false", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.work", Label: "MCP work 권한", Category: "mcp", Type: "bool", Default: "true", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.admin_read", Label: "MCP admin_read 권한", Category: "mcp", Type: "bool", Default: "false", Scope: "admin", Apply: "live", Lockable: false},
		{Key: "mcp.permissions.admin_write", Label: "MCP admin_write 권한", Category: "mcp", Type: "bool", Default: "false", Scope: "admin", Apply: "live", Lockable: false},
	}
	result = append(result, extra...)
	result = append(result, notifyMailDefinitions()...)
	for _, key := range tracking.SettingKeys {
		d := SettingDefinition{Key: key, Label: key, Category: "security", Type: "string", Default: tracking.Defaults[key], Scope: "admin", Apply: "live"}
		if strings.HasSuffix(key, "enabled") {
			d.Type = "bool"
		}
		if strings.HasSuffix(key, "snippet") {
			d.Type = "text"
			d.Help = "추적 스크립트는 사용자 메일 데이터에 접근하지 못하도록 별도 격리 정책을 적용합니다."
		}
		result = append(result, d)
	}
	return result
}

// notifyMailDefinitions are the relay notification settings (MAIL-STANDARD).
// The keys are shared with every other internal service; the password is a
// write-only secret whose stored value is a SecretStore reference.
func notifyMailDefinitions() []SettingDefinition {
	def := func(key, label, typ string) SettingDefinition {
		return SettingDefinition{Key: key, Label: label, Category: "notifications", Type: typ, Default: notifymail.Defaults[key], Scope: "admin", Apply: "live"}
	}
	out := []SettingDefinition{
		def(notifymail.KeyEnabled, "알림 메일 발송 (SMTP 릴레이)", "bool"),
		def(notifymail.KeyHost, "알림 SMTP 릴레이 주소", "string"),
		def(notifymail.KeyPort, "알림 SMTP 포트", "int"),
		def(notifymail.KeySecurity, "알림 SMTP 보안", "enum"),
		def(notifymail.KeySkipTLSVerify, "알림 SMTP 인증서 검증 생략", "bool"),
		def(notifymail.KeyUsername, "알림 SMTP 사용자 이름 (선택)", "string"),
		def(notifymail.KeyPassword, "알림 SMTP 비밀번호 (선택)", "secret"),
		def(notifymail.KeyFromAddress, "알림 보내는 주소", "string"),
		def(notifymail.KeyFromName, "알림 보내는 이름", "string"),
		def(notifymail.KeyBaseURL, "알림 메일 속 링크 기준 URL", "url"),
		def(notifymail.KeyTimeout, "알림 SMTP 제한 시간(초)", "int"),
		def(notifymail.NotifyKey(notifymail.EventSendFailed), "알림: 발송 실패로 멈춤", "bool"),
		def(notifymail.NotifyKey(notifymail.EventAssigned), "알림: 담당 배정", "bool"),
		def(notifymail.NotifyKey(notifymail.EventSyncCredentialError), "알림: 수집 인증 실패", "bool"),
		def(notifymail.NotifyKey(notifymail.EventIncident), "알림: 심각 장애 (관리자)", "bool"),
	}
	for i := range out {
		switch out[i].Key {
		case notifymail.KeyEnabled:
			out[i].Help = "사내 SMTP 릴레이로 이벤트 알림을 보냅니다. 기본은 꺼짐이며, 릴레이는 대개 포트 25·인증 없음·TLS 없음입니다."
		case notifymail.KeySecurity:
			out[i].Options = notifymail.SecurityOptions
			out[i].Help = "auto 는 서버가 STARTTLS 를 알리면 쓰고 아니면 평문으로 보냅니다. 465 포트는 자동으로 tls 입니다."
		case notifymail.KeyPassword:
			out[i].Secret = true
			out[i].Help = "인증이 없는 릴레이는 비워 둡니다. 저장한 비밀번호는 다시 조회할 수 없습니다."
		case notifymail.KeyBaseURL:
			out[i].Help = "메일 속 '바로 열기' 링크가 가리킬 이 앱의 공개 주소입니다. 비우면 링크를 넣지 않습니다."
		case notifymail.KeySkipTLSVerify:
			out[i].Help = "사내 인증서가 사설일 때만 켭니다."
		}
	}
	return out
}

func settingDefinition(key string) (SettingDefinition, bool) {
	for _, d := range SettingsDefinitions() {
		if d.Key == key {
			return d, true
		}
	}
	return SettingDefinition{}, false
}

func validateSetting(d SettingDefinition, value string) error {
	fail := func() error { return userErrf("설정 %s 값이 올바르지 않습니다", d.Key) }
	if len(value) > 65536 {
		return fail()
	}
	switch d.Type {
	case "bool":
		if value != "true" && value != "false" {
			return fail()
		}
	case "int":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 || n > 1<<40 {
			return fail()
		}
		switch d.Key {
		case "auth.session_hours":
			if n < 1 || n > 8760 {
				return fail()
			}
		case "ai.max_tokens":
			if n > 10000000 {
				return fail()
			}
		case "sync.max_concurrent_syncs":
			if n < 1 || n > 64 {
				return fail()
			}
		case "sync.min_sync_minutes":
			if n < 1 || n > 10080 {
				return fail()
			}
		case "sync.daily_digest_hour":
			if n > 23 {
				return fail()
			}
		case "ui.preview_lines":
			if n > 5 {
				return fail()
			}
		case "ai.timeout_sec", "mcp.request_timeout_sec":
			if n < 1 || n > 3600 {
				return fail()
			}
		case "mcp.session_timeout_sec":
			if n < 60 || n > 604800 {
				return fail()
			}
		case "ai.context_length":
			if n < 128 || n > 10000000 {
				return fail()
			}
		case "notifications.poll_seconds":
			if n < 5 || n > 300 {
				return fail()
			}
		case notifymail.KeyPort:
			if n < 1 || n > 65535 {
				return fail()
			}
		case notifymail.KeyTimeout:
			if n < 1 || n > 300 {
				return fail()
			}
		}
	case "number":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return fail()
		}
		if d.Key == "ai.temperature" && n > 2 {
			return fail()
		}
	case "enum":
		found := false
		for _, option := range d.Options {
			if value == option {
				found = true
			}
		}
		if !found {
			return fail()
		}
	case "json":
		if value != "" && !json.Valid([]byte(value)) {
			return fail()
		}
	case "url":
		if value != "" {
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
				return userErrf("설정 %s은 인증정보나 쿼리 문자열이 없는 HTTP(S) URL이어야 합니다", d.Key)
			}
		}
	}
	if d.Key == notifymail.KeyFromAddress && value != "" {
		if _, err := mail.ParseAddress(value); err != nil || strings.ContainsAny(value, "<>\r\n") {
			return userErrf("설정 %s 은 이름 없는 메일 주소여야 합니다", d.Key)
		}
	}
	if (d.Key == notifymail.KeyHost || d.Key == notifymail.KeyUsername) && strings.ContainsAny(value, " \t\r\n") {
		return fail()
	}
	if d.Key == notifymail.KeyFromName && strings.ContainsAny(value, "\r\n") {
		return fail()
	}
	if d.Key == "ai.task_models" && value != "" {
		var routes map[string]config.AITaskRoute
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&routes); err != nil || routes == nil {
			return fail()
		}
		for task, route := range routes {
			if task == "" || len(task) > 100 || route.MaxTokens < 0 || route.MaxTokens > 10000000 {
				return fail()
			}
			if route.APIKeyRef != "" && (!strings.HasPrefix(route.APIKeyRef, "sec_") || len(route.APIKeyRef) > 128 || strings.ContainsAny(route.APIKeyRef, " \t\r\n")) {
				return userErrf("작업별 인증은 등록된 Secret 참조만 지정하세요. API Key 원문은 저장하지 마세요")
			}
			if err := validateSetting(SettingDefinition{Key: "ai.task_models.base_url", Type: "url"}, route.BaseURL); err != nil {
				return err
			}
		}
	}
	if d.Key == "mcp.policy" {
		if err := ValidateMCPPolicySettings(value); err != nil {
			return err
		}
	}
	if d.Key == "ai.extra_headers" && value != "" {
		if err := validateExtraHeaders(value); err != nil {
			return err
		}
	}
	if d.Key == "mcp.endpoint" && (value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "?#\\ \t\r\n") || strings.HasPrefix(value, "/app") || strings.HasPrefix(value, "/auth") || strings.HasPrefix(value, "/api") || value == "/") {
		return fail()
	}
	if d.Key == "mcp.endpoint" {
		for _, reserved := range []string{"/metrics", "/healthz", "/readyz", "/livez", "/ui", "/tracking", "/momento", "/favicon.ico", "/favicon.png", "/logo.png"} {
			if value == reserved || strings.HasPrefix(value, reserved+"/") {
				return fail()
			}
		}
	}
	return nil
}

func validateExtraHeaders(value string) error {
	var headers map[string]string
	if err := json.Unmarshal([]byte(value), &headers); err != nil || headers == nil {
		return userErrf("추가 API 헤더는 문자열 값으로 된 JSON 객체여야 합니다")
	}
	for key, value := range headers {
		if strings.TrimSpace(key) != key || key == "" || strings.ContainsAny(key, " :\r\n\t") || strings.ContainsAny(value, "\r\n") {
			return userErrf("올바르지 않은 API 헤더입니다")
		}
		switch http.CanonicalHeaderKey(key) {
		case "Authorization", "Host", "Content-Length", "Transfer-Encoding", "Connection", "Content-Type", "Accept":
			return userErrf("인증 헤더는 API Key 항목을 사용하고 전송 제어 헤더는 지정하지 마세요")
		}
	}
	return nil
}

func validateSettingValues(values map[string]string) error {
	for key, value := range values {
		if key == "vector.milvus_token" {
			continue
		}
		if strings.HasPrefix(key, "policy.lock.") {
			d, ok := settingDefinition(strings.TrimPrefix(key, "policy.lock."))
			if !ok || !d.Lockable || (value != "true" && value != "false") {
				return userErrf("지원하지 않는 강제 정책입니다")
			}
			continue
		}
		d, ok := settingDefinition(key)
		if !ok {
			return userErrf("지원하지 않는 설정 키입니다: %s", key)
		}
		if d.Apply == "deployment" {
			return userErrf("설정 %s은 배포 환경에서 변경해야 합니다", key)
		}
		if d.Apply == "fixed" {
			return userErrf("설정 %s은 항상 적용되는 고정 정책이며 변경할 수 없습니다", key)
		}
		if d.Type == "account" || d.Type == "signature" {
			return userErrf("계정과 서명 선택은 사용자 개인 설정에서 지정하세요")
		}
		if err := validateSetting(d, value); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	for _, d := range SettingsDefinitions() {
		if d.Apply != "deployment" && d.Apply != "fixed" {
			allowedSettings[d.Key] = true
		}
		if d.Lockable {
			allowedSettings["policy.lock."+d.Key] = true
		}
	}
}

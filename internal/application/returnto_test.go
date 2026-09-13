package application

import "testing"

// SafeReturnTo 는 로그인 뒤 돌려보낼 곳을 정합니다. 값이 사용자에게서
// 오므로, 여기서 막지 못하면 로그인 링크 하나로 방문자를 바깥 사이트로
// 보낼 수 있습니다. 막아야 하는 모양을 눈으로 확인해 둡니다.
func TestSafeReturnToRejectsOffSiteTargets(t *testing.T) {
	for _, v := range []string{
		"//evil.example",           // 프로토콜 상대 — 브라우저는 바깥으로 간다
		"///evil.example",          //
		"https://evil.example",     // 절대 URL
		"http://evil.example",      //
		"/\\evil.example",          // 역슬래시로 "//" 를 흉내내는 수
		"\\\\evil.example",         //
		"evil.example",             // "/" 로 시작하지 않음
		"javascript:alert(1)",      // 스킴
		"/path\r\nSet-Cookie: a=1", // 날것의 CR/LF
		"",                         // 빈 값
	} {
		if SafeReturnTo(v) {
			t.Errorf("바깥으로 보낼 수 있는 값을 통과시켰습니다: %q", v)
		}
	}
}

func TestSafeReturnToKeepsInAppPaths(t *testing.T) {
	for _, v := range []string{"/", "/ui/threads/1", "/ui/x?y=1#z", "/a%20b"} {
		if !SafeReturnTo(v) {
			t.Errorf("같은 오리진 경로를 막았습니다: %q", v)
		}
	}
}

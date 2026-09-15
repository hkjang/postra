package receivedhtml

import (
	"strings"
	"testing"
)

func TestReceivedImagesAreInertUntilAllowed(t *testing.T) {
	input := `<h2 style="color:#224488">한글 본문</h2><img src="https://images.corp.local/photo.png?a=1&amp;b=2" width="400" onerror="alert(1)"><img src="cid:inline1"><script>alert('bad')</script><a href="javascript:alert(1)">링크</a>`
	stored := Sanitize(input)
	if !strings.HasPrefix(stored, Marker) || !strings.Contains(stored, `data-postra-image="https://images.corp.local/photo.png?a=1&amp;b=2"`) || strings.Contains(stored, `src=`) || strings.Contains(stored, "onerror") || strings.Contains(stored, "javascript:") || strings.Contains(stored, "<script") {
		t.Fatalf("unsafe stored mail: %s", stored)
	}
	blocked, count := Render(stored, false)
	if count != 1 || strings.Contains(blocked, `src=`) {
		t.Fatalf("blocked render: %s", blocked)
	}
	allowed, count := Render(stored, true)
	if count != 1 || !strings.Contains(allowed, `src="https://images.corp.local/photo.png?a=1&amp;b=2"`) || strings.Contains(allowed, `src="cid:`) {
		t.Fatalf("allowed render: %s", allowed)
	}
}

func TestReceivedImagesRejectPixelsDataSVGAndCSSLoads(t *testing.T) {
	for _, tag := range []string{
		`<img src="data:image/svg+xml;base64,abc">`, `<img src="javascript:alert(1)">`, `<img src="//other.test/photo.png">`,
		`<img src="https://other.test/danger.svg">`, `<img src="https://other.test/tracking.gif">`, `<img src="https://other.test/beacon/open.png">`,
		`<img src="https://other.test/x.png" width="1">`, `<img src="https://other.test/x.png" height="0">`, `<img src="https://other.test/x.png" style="width:1px">`,
		`<img src="https://other.test/x.png" style="display:none">`, `<img src="https://name:password@other.test/x.png">`, `<img src="https://other.test/x.png" style="opacity:0">`,
	} {
		if output, count := Render(tag, true); count != 0 || strings.Contains(output, "<img") {
			t.Errorf("unsafe image retained from %q: %s", tag, output)
		}
	}
	output, _ := Render(`<div style="background-image:url(https://evil.test/x.png);color:red">safe</div><video poster="https://evil.test/v.png"></video><svg><image href="https://evil.test/a"></svg><iframe src="https://evil.test"></iframe><img src="https://safe.test/image.png" srcset="https://evil.test/tracking.png 2x">`, true)
	if strings.Contains(output, "evil.test") || strings.Contains(output, "<svg") || strings.Contains(output, "<iframe") {
		t.Fatalf("active untrusted resource: %s", output)
	}
}

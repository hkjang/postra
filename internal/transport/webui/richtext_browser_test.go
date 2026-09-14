package webui

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

// Optional real-browser regression. No mail leaves this process: the server
// uses a temporary database and the same fake SMTP as the HTTP tests.
func TestRichMailBrowser(t *testing.T) {
	if os.Getenv("POSTRA_BROWSER_TEST") != "1" {
		t.Skip("set POSTRA_BROWSER_TEST=1 and install Playwright to run browser smoke")
	}
	app, smtp := newTestApp(t)
	richMailAccount(t, app)
	srv := httptest.NewServer(New(app, "").Handler())
	defer srv.Close()
	cmd := exec.Command("node", "../../../scripts/test-rich-mail.cjs")
	cmd.Env = append(os.Environ(), "POSTRA_TEST_URL="+srv.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser smoke: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	if smtp.sent != 1 {
		t.Fatalf("browser should explicitly approve/send once, got %d", smtp.sent)
	}
}

// Run via POSTRA_BROWSER_TEST=1 go test ./internal/transport/webui -run '^TestRichMailBrowser$' -v
// Optional: POSTRA_PLAYWRIGHT_MODULE=/path/to/playwright, POSTRA_CHROME=/path/to/chrome
const assert = require('node:assert/strict');
const {chromium} = require(process.env.POSTRA_PLAYWRIGHT_MODULE || 'playwright');

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.POSTRA_CHROME ? {executablePath: process.env.POSTRA_CHROME} : {})});
  try {
    const page = await browser.newPage({viewport: {width: 1200, height: 1050}});
    const errors = [];
    const requests = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('request', request => requests.push(request.url()));
    page.on('dialog', dialog => dialog.type() === 'beforeunload' ? dialog.accept() : dialog.dismiss());
    const base = process.env.POSTRA_TEST_URL;
    await page.goto(base + '/ui/compose');
    await page.waitForSelector('[data-mail-editor][data-ready="true"]');
    await page.locator('[name=to]').fill('team@corp.local');
    await page.locator('[name=subject]').fill('HTML 메일 브라우저 검증');
    const editor = page.frameLocator('[data-editor-frame]').locator('body');
    await editor.fill('팀 여러분 안녕하세요.\n이번 주 진행 상황을 공유합니다.');
    await editor.press('Control+a');
    await page.locator('[data-command=bold]').click();
    await page.locator('[data-color]').fill('#b42344');
    assert.match(await editor.innerHTML(), /color:\s*(rgb\(180, 35, 68\)|#b42344)/);
    await editor.press('Control+Home');
    await editor.press('Shift+End');
    await page.locator('[data-show-link]').click();
    await page.locator('[data-link-url]').fill('https://example.com/team');
    await page.locator('[data-insert-link]').click();
    assert.match(await editor.innerHTML(), /href="https:\/\/example.com\/team"/);
    await page.locator('[data-theme=notice]').click();
    assert.match(await editor.innerHTML(), /font-weight:\s*(bold|700)/);
    assert.match(await editor.innerHTML(), /안내드립니다/);
    // Switching templates should replace the wrapper, not nest the prior theme.
    await page.locator('[data-theme=report]').click();
    assert.doesNotMatch(await editor.innerHTML(), /안내드립니다/);
    assert.match(await editor.innerHTML(), /업무 공유/);
    await page.locator('[data-editor-mode]').selectOption('source');
    let html = await page.locator('[data-html-source]').inputValue();
    assert.match(html, /팀 여러분/);
    await page.locator('[data-html-source]').fill(html + '<p><a href="https://example.com/report">보고서 보기</a></p><script>window.mailXSS=1</script><img src="https://example.com/tracker" onerror="window.mailXSS=1">');
    await page.locator('[data-editor-mode]').selectOption('html');
    assert.doesNotMatch(await editor.innerHTML(), /script|onerror|tracker/);
    assert.equal(await page.evaluate(() => window.mailXSS), undefined);
    await page.locator('[data-show-link]').click();
    await page.locator('[data-link-url]').fill('not a url');
    await page.locator('[data-close-link]').click();
    await page.getByRole('button', {name: '초안 만들기', exact: true}).click();
    await page.waitForURL(/\/ui\/drafts\/[a-zA-Z0-9_-]+$/);
    await page.waitForSelector('[data-mail-editor][data-ready="true"]');
    assert.match(await editor.innerHTML(), /업무 공유/);
    assert.match(await editor.innerHTML(), /background-color/);
    // Existing HTML survives reload and the actual save form.
    await page.getByRole('button', {name: '변경 저장', exact: true}).click();
    await page.waitForURL(/saved=1/);
    await page.getByRole('link', {name: '발송 미리보기·승인 →'}).click();
    const preview = page.frameLocator('iframe[title="서식 메일 미리보기"]');
    await preview.getByText('업무 공유', {exact: true}).waitFor();
    assert.match(await preview.locator('body').innerText(), /팀 여러분/);
    assert.equal(await page.locator('.outgoing-preview').getAttribute('sandbox'), '');
    if (process.env.POSTRA_BROWSER_SCREENSHOT) await page.screenshot({path: process.env.POSTRA_BROWSER_SCREENSHOT, fullPage: true});
    await page.getByRole('button', {name: '이 내용으로 승인 요청'}).click();
    await page.getByRole('button', {name: '발송 확정', exact: true}).click();
    await page.getByRole('heading', {name: /발송/}).first().waitFor();
    // HTML → plain mode really clears HTML, including after reloading.
    await page.goto(base + '/ui/compose');
    await page.waitForSelector('[data-mail-editor][data-ready="true"]');
    await page.locator('[name=to]').fill('team@corp.local');
    await page.locator('[name=subject]').fill('일반 텍스트 전환');
    await editor.fill('일반 텍스트로 전환할 내용');
    await editor.press('Control+a');
    await page.locator('[data-command=insertUnorderedList]').click();
    assert.match(await editor.innerHTML(), /<ul>/);
    await page.locator('[data-theme=letter]').click();
    await page.locator('[data-editor-mode]').selectOption('plain');
    assert.match(await page.locator('[data-plain-source]').inputValue(), /일반 텍스트로 전환할 내용/);
    assert.equal(await page.locator('[data-html-source]').inputValue(), '');
    await page.getByRole('button', {name: '초안 만들기', exact: true}).click();
    await page.waitForSelector('[data-mail-editor][data-ready="true"]');
    assert.equal(await page.locator('[data-editor-mode]').inputValue(), 'plain');
    await page.setViewportSize({width: 390, height: 844});
    await page.locator('[data-editor-mode]').selectOption('html');
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
    assert.equal(overflow, false, 'editor must fit mobile viewport');
    assert.deepEqual(errors, [], 'no browser runtime errors');
    assert.deepEqual(requests.filter(url => !url.startsWith(base) && !url.startsWith('about:')), [], 'editor must not access external resources');
    console.log('PASS: formatting, templates, source sanitization, persistence, approval/send, plain mode, mobile, offline requests');
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });

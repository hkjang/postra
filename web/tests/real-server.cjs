// Driven by TestSPABrowser: real Go auth/API/storage, in-process fake mail + AI.
// npm dependencies are development-only and are never required on offline hosts.
const assert = require('node:assert/strict');
const {chromium, expect} = require('@playwright/test');

(async () => {
  const browser = await chromium.launch({headless: true, ...(process.env.POSTRA_CHROME ? {executablePath: process.env.POSTRA_CHROME} : {})});
  const context = await browser.newContext({viewport: {width: 1520, height: 1050}, ignoreHTTPSErrors: true}); // local httptest TLS certificate only
  const page = await context.newPage();
  const base = process.env.POSTRA_TEST_URL;
  const errors = [];
  const external = [];
  page.on('pageerror', error => errors.push(error.message));
  const clickedFixtureURLs = new Set();
  context.on('request', request => {if (!request.url().startsWith(base) && !request.url().startsWith('about:') && !request.url().startsWith('data:') && !clickedFixtureURLs.has(request.url())) external.push(request.url());});
  page.on('dialog', dialog => dialog.type() === 'beforeunload' ? dialog.accept() : dialog.dismiss());
  async function responsiveScreenshots(name) {
    for (const width of [1520, 390, 320]) {
      await page.setViewportSize({width, height: width === 1520 ? 1050 : 844});
      if (width < 720) await expect.poll(() => page.locator('.sidebar').evaluate(element => element.getBoundingClientRect().right)).toBeLessThanOrEqual(0);
      await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth), {message: `${name} at ${width}px has no horizontal overflow`}).toBeLessThanOrEqual(0);
      if (name === 'work-large' && width < 720) {
        for (const label of ['상태 필터', '업무 정렬']) await expect.poll(() => page.getByRole('combobox', {name: label, exact: true}).evaluate(element => element.getBoundingClientRect().width), {message: `${label} stays readable instead of collapsing to its arrow at ${width}px`}).toBeGreaterThanOrEqual(200);
      }
      if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: `${process.env.POSTRA_SPA_SCREENSHOT_DIR}/${name}-${width}.png`, fullPage: true, animations: 'disabled'});
    }
    await page.setViewportSize({width: 1520, height: 1050});
  }
  async function saveTextSize(value) {
    await page.getByRole('combobox', {name: '화면 글자 크기', exact: true}).selectOption(value);
    await page.getByRole('button', {name: '변경 확인', exact: true}).click();
    const review = page.getByRole('dialog', {name: '저장할 변경사항'});
    await expect(review).toContainText('화면 글자 크기');
    await review.getByRole('button', {name: '확인 후 저장', exact: true}).click();
    await expect(review).toHaveCount(0);
    await expect(page.locator('html')).toHaveAttribute('data-text-size', value);
    await expect(page.getByRole('button', {name: '변경 확인', exact: true})).toBeDisabled();
  }
  try {
    await page.goto(base + '/app/');
    // A genuine user with zero stored actions used to crash at cards.filter.
    // No API interception: prove the real backend's empty result and navigate
    // all empty views before the seeded administrator scenario starts.
    await page.locator('[name=login_id]').fill(process.env.POSTRA_TEST_OTHER_LOGIN);
    await page.locator('[name=password]').fill(process.env.POSTRA_TEST_OTHER_PASSWORD);
    await page.getByRole('button', {name: '계정으로 로그인', exact: true}).click();
    await page.waitForURL(/\/app\/mail/);
    const emptyActions = await page.evaluate(async () => (await fetch('/api/action-cards?limit=200')).json());
    assert.deepEqual(emptyActions.cards, [], 'real empty action collection is []');
    await page.keyboard.press('Control+k');
    await page.getByRole('option', {name: '액션 센터', exact: true}).click();
    await page.getByText('아직 등록된 액션이 없습니다', {exact:true}).waitFor();
    for (const status of ['pending','approved','done','rejected','exported','']) {
      await page.getByRole('combobox',{name:'승인·완료 상태'}).selectOption(status);
      await page.getByText(status ? '조건에 맞는 액션이 없습니다' : '아직 등록된 액션이 없습니다',{exact:true}).waitFor();
    }
    for (const button of await page.getByLabel('액션 기한 분류').getByRole('button').all()) await button.click();
    await page.keyboard.press('j'); await page.keyboard.press('k');
    await page.goto(base+'/app/mail?q=no-such-empty-fixture-term');
    await page.getByText('검색 결과가 없습니다',{exact:true}).waitFor();
    await page.keyboard.press('j'); await page.keyboard.press('k');
    await page.keyboard.press('Control+k');
    await page.getByRole('option', {name:'액션 센터',exact:true}).click();
    await page.getByText('아직 등록된 액션이 없습니다',{exact:true}).waitFor();
    assert.deepEqual(errors, [], 'empty action and search routes must not produce pageerror');
    await page.getByRole('button',{name:'로그아웃',exact:true}).click();
    await page.waitForURL(url=>url.searchParams.get('sso')==='signed_out');
    await page.locator('[name=login_id]').fill(process.env.POSTRA_TEST_LOGIN);
    await page.locator('[name=password]').fill(process.env.POSTRA_TEST_PASSWORD);
    await page.getByRole('button', {name: '계정으로 로그인', exact: true}).click();
    await page.waitForURL(/\/app\/mail/);
    await page.getByRole('heading', {name: '받은메일', exact: true}).waitFor();
    await page.getByRole('button').filter({hasText: '프로젝트 킥오프 일정 공유'}).click();
    await page.getByRole('heading', {name: '프로젝트 킥오프 일정 공유'}).waitFor();
    await page.getByRole('heading', {name: 'AI Insight'}).waitFor();
    const receivedFrame = page.frameLocator('iframe[title="메일 본문"]');
    await expect(receivedFrame.getByRole('link',{name:'HTTP 안내',exact:true})).toHaveAttribute('target','_blank');
    assert.equal(await page.locator('iframe[title="메일 본문"]').getAttribute('sandbox'),'allow-popups allow-popups-to-escape-sandbox');
    assert.equal(await receivedFrame.locator('script,form,a[href^="javascript:"]').count(),0);
    assert.equal(await receivedFrame.locator('img[src]').count(),0,'remote images remain blocked while safe links work');
    const parentURL=page.url();
    async function openLink(locator,url) {
      clickedFixtureURLs.add(url);
      const popupPromise=page.waitForEvent('popup'); await locator.click(); const popup=await popupPromise;
      popup.on('pageerror',error=>errors.push(error.message));
      await popup.waitForURL(url); await expect(popup).toHaveTitle('LINK_OPENED');
      assert.equal(await popup.evaluate(()=>window.opener),null,'clicked link cannot access the mail workspace');
      assert.equal(await popup.evaluate(()=>document.referrer),'','clicked link has no referrer');
      await popup.close();
    }
    await openLink(receivedFrame.getByRole('link',{name:'HTTP 안내',exact:true}),process.env.POSTRA_TEST_LINK_HTTP);
    await openLink(receivedFrame.getByRole('link',{name:'HTTPS 안내',exact:true}),process.env.POSTRA_TEST_LINK_HTTPS);
    assert.equal(page.url(),parentURL,'mail links must never replace the workspace');
    await expect(receivedFrame.getByRole('link',{name:'메일 문의',exact:true})).toHaveAttribute('href','mailto:help@corp.local?subject=Hello');
    await expect(receivedFrame.getByRole('link',{name:'메일 문의',exact:true})).toHaveAttribute('target','_blank');
    await page.goto(base+'/app/messages/msg_spa_important');
    await openLink(page.locator('.mail-body-text').getByRole('link',{name:process.env.POSTRA_TEST_LINK_HTTP,exact:true}),process.env.POSTRA_TEST_LINK_HTTP);
    await openLink(page.locator('.mail-body-text').getByRole('link',{name:process.env.POSTRA_TEST_LINK_HTTPS,exact:true}),process.env.POSTRA_TEST_LINK_HTTPS);
    await expect(page.locator('.mail-body-text').getByRole('link',{name:'help@corp.local'})).toHaveAttribute('href','mailto:help@corp.local');
    await page.goto(parentURL);
    await page.getByRole('heading', {name: 'AI Insight'}).waitFor();
    await page.getByRole('button', {name: '요약하기', exact: true}).click();
    await expect(page.locator('.ai-panel')).toContainText('회의 자료 검토');
    assert.doesNotMatch(await page.locator('body').innerText(), new RegExp(process.env.POSTRA_TEST_PRIVATE_SUBJECT));
    const privateStatus = await page.evaluate(async id => (await fetch('/api/messages/' + id)).status, process.env.POSTRA_TEST_OTHER_MESSAGE_ID);
    assert.equal(privateStatus, 404, 'admin still cannot read another user mailbox');
    const noCSRF = await page.evaluate(async () => (await fetch('/api/drafts', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'})).status);
    assert.equal(noCSRF, 403, 'cookie writes require CSRF');
    assert.doesNotMatch(await page.evaluate(() => document.cookie), /postra_session=/, 'session must be HttpOnly');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/inbox-light.png', fullPage: true});
    await page.getByRole('button', {name: '어두운 테마', exact: true}).click();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/inbox-dark.png', fullPage: true});
    await page.getByRole('button', {name: '밝은 테마', exact: true}).click();
    // Persisted personal typography uses the actual preference API. The same
    // live value must survive a reload and scale controls and metadata evenly.
    await page.goto(base + '/app/settings');
    await expect(page.getByRole('combobox', {name: '화면 글자 크기', exact: true})).toHaveValue('standard');
    await page.getByRole('textbox', {name: '설정 검색', exact: true}).fill('화면 글자 크기');
    await saveTextSize('large');
    for (const control of [page.getByRole('textbox', {name: '설정 검색', exact: true}), page.getByRole('combobox', {name: '화면 글자 크기', exact: true}), page.getByRole('button', {name: '변경 확인', exact: true})]) {
      await expect(control).toHaveCSS('font-size', '16.875px');
    }
    await expect(page.locator('.page-header p')).toHaveCSS('font-size', '14.625px');
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-text-size', 'large');
    await expect(page.getByRole('combobox', {name: '화면 글자 크기', exact: true})).toHaveValue('large');
    await page.getByRole('textbox', {name: '설정 검색', exact: true}).fill('화면 글자 크기');
    await responsiveScreenshots('settings-large');
    await page.goto(base + '/app/mail');
    await expect(page.locator('.mail-list')).toContainText('프로젝트 킥오프 일정 공유');
    await expect(page.locator('.mail-item-top time').first()).toHaveCSS('font-size', '14.625px');
    await responsiveScreenshots('inbox-large');
    await page.goto(base + '/app/actions');
    await expect(page.getByRole('heading', {name: '회의 자료 검토', exact: true})).toBeVisible();
    await responsiveScreenshots('actions-large');
    await page.goto(base + '/app/team?message=' + process.env.POSTRA_TEST_MESSAGE_ID);
    await page.getByRole('heading', {name: '처리 관리', exact: true}).waitFor();
    await responsiveScreenshots('work-large');
    await page.goto(base + '/app/compose');
    await page.getByRole('heading', {name: '새 메일 작성', exact: true}).waitFor();
    await expect(page.getByRole('textbox', {name: '받는 사람', exact: true})).toHaveCSS('font-size', '16.875px');
    await responsiveScreenshots('compose-large');
    // Reply previews must be lazy and read-only: no read-marker mutation, AI
    // generation, draft rewrite, attachment import, or extra SMTP send.
    await page.goto(parentURL);
    await expect(page.locator('html')).toHaveAttribute('data-text-size', 'large');
    await page.evaluate(() => document.fonts.ready);
    const replyButton = page.getByRole('button', {name: '답장', exact: true});
    await expect(replyButton).toBeEnabled();
    await replyButton.scrollIntoViewIfNeeded();
    let previousReplyBox = '', stableReplyBox = 0;
    await expect.poll(async () => {
      const box = JSON.stringify(await replyButton.boundingBox());
      stableReplyBox = box !== 'null' && box === previousReplyBox ? stableReplyBox + 1 : 0;
      previousReplyBox = box;
      return stableReplyBox;
    }, {message: 'reply button settles after persisted typography and iframe layout'}).toBeGreaterThanOrEqual(2);
    await expect.poll(() => replyButton.evaluate(element => {
      const rect = element.getBoundingClientRect();
      return element.contains(document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2));
    }), {message: 'reply click point belongs to the button, not the mail iframe'}).toBe(true);
    const sourceRequests = [], sourceMutations = [];
    const trackSource = request => {
      const url = new URL(request.url());
      if (url.origin !== new URL(base).origin) return;
      if (url.pathname === '/api/messages/' + process.env.POSTRA_TEST_MESSAGE_ID) sourceRequests.push(request.method());
      if (url.pathname.startsWith('/api/') && !['GET', 'HEAD'].includes(request.method())) sourceMutations.push(`${request.method()} ${url.pathname}`);
    };
    context.on('request', trackSource);
    await page.evaluate(() => {
      window.__postraReplyPointerEvents = [];
      for (const type of ['pointerdown', 'pointerup', 'click']) document.addEventListener(type, event => {
        const target = event.target;
        window.__postraReplyPointerEvents.push({type, tag: target?.tagName, button: target?.closest?.('button')?.getAttribute('title') || ''});
      }, {capture: true, once: true});
    });
    await replyButton.click();
    await page.waitForURL(/\/app\/drafts\//).catch(async error => {
      const pointerEvents = await page.evaluate(() => window.__postraReplyPointerEvents);
      throw new Error(`Reply navigation failed; pointer=${JSON.stringify(pointerEvents)}, source reads=${JSON.stringify(sourceRequests)}, API writes=${JSON.stringify(sourceMutations)}: ${error.message}`);
    });
    await page.getByRole('heading', {name: '메일 초안', exact: true}).waitFor();
    sourceMutations.length = 0; // The explicit reply-draft creation is expected.
    const source = page.getByRole('region', {name: '답장 원문', exact: true});
    await expect(source.getByRole('button', {name: '원문 펼치기', exact: true})).toHaveAttribute('aria-expanded', 'false');
    assert.deepEqual(sourceRequests, [], 'collapsed source preview does not fetch the message');
    const composeFields = page.locator('.compose-fields');
    const readFields = () => composeFields.evaluate(element => [...element.querySelectorAll('input,select,textarea,[contenteditable=true]')].map(control => ({label: control.getAttribute('aria-label'), value: control.isContentEditable ? control.innerHTML : control.value})));
    const fieldsBeforeSource = await readFields();
    await source.getByRole('button', {name: '원문 펼치기', exact: true}).click();
    await expect(source).toContainText('프로젝트 킥오프에 앞서 공유된 회의 자료를 검토');
    await expect(source).not.toContainText(process.env.POSTRA_TEST_PRIVATE_SUBJECT);
    await expect(source.getByRole('link', {name: '원본 메일 새 탭에서 열기'})).toHaveAttribute('target', '_blank');
    await expect(source.getByRole('link', {name: '원본 메일 새 탭에서 열기'})).toHaveAttribute('rel', 'noopener noreferrer');
    assert.equal(await source.locator('script,form,iframe,img[src]').count(), 0, 'plain source is safely rendered without external media');
    await responsiveScreenshots('reply-source-large');
    await source.getByRole('button', {name: '원문 접기', exact: true}).click();
    await expect(source.getByRole('button', {name: '원문 펼치기', exact: true})).toHaveAttribute('aria-expanded', 'false');
    assert.deepEqual(await readFields(), fieldsBeforeSource, 'viewing the original never inserts or changes draft content');
    assert.deepEqual(sourceRequests, ['GET'], 'original preview performs exactly one read-only fetch');
    assert.deepEqual(sourceMutations, [], 'viewing the original does not mutate messages or drafts or start AI jobs');
    context.off('request', trackSource);
    await page.goto(base + '/app/settings');
    await saveTextSize('standard');
    await expect(page.getByRole('textbox', {name: '설정 검색', exact: true})).toHaveCSS('font-size', '15px');
    await expect(page.locator('.page-header p')).toHaveCSS('font-size', '13px');
    await page.goto(parentURL);
    // Follow-up workflow uses real authenticated APIs, never AI or SMTP.
    await page.getByRole('button', {name: '나중에 다시 보기', exact: true}).click();
    await page.getByRole('combobox', {name: '다시 볼 시각', exact: true}).selectOption('custom');
    await page.getByLabel('직접 지정 날짜·시각', {exact: true}).fill('2050-01-02T09:00');
    await page.getByRole('button', {name: '다시 보기 예약', exact: true}).click();
    await expect(page.getByRole('dialog', {name: '메일을 나중에 다시 보기'})).toHaveCount(0);
    const reminder = await page.evaluate(async id => (await (await fetch('/api/messages/' + encodeURIComponent(id) + '?body=false')).json()).message.snoozed_until, process.env.POSTRA_TEST_MESSAGE_ID);
    assert.ok(reminder > Date.now()/1000, 'custom reminder is saved by the real API');
    await page.getByRole('button', {name: '다시 보기 변경', exact: true}).click();
    await page.getByRole('button', {name: '예약 해제', exact: true}).click();
    await expect(page.getByRole('button', {name: '나중에 다시 보기', exact: true})).toBeVisible();
    await page.getByRole('link', {name: '이 메일에서 액션 만들기', exact: true}).click();
    await expect(page.getByLabel('액션 제목', {exact: true})).toHaveValue('프로젝트 킥오프 일정 공유');
    await page.getByRole('button', {name: '다른 메일 선택', exact: true}).click();
    await expect(page.getByRole('region', {name: '액션 원본 메일 선택'})).not.toContainText(process.env.POSTRA_TEST_PRIVATE_SUBJECT);
    await page.getByRole('button', {name: '프로젝트 킥오프 일정 공유 · 김민수 선택', exact: true}).click();
    await page.setViewportSize({width: 390, height: 844});
    await expect.poll(() => page.locator('.sidebar').evaluate(element => element.getBoundingClientRect().right)).toBeLessThanOrEqual(0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'mobile action form has no horizontal overflow');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/action-create-mobile.png', fullPage: true, animations: 'disabled'});
    await page.setViewportSize({width: 1520, height: 1050});
    await page.getByLabel('액션 제목', {exact: true}).fill('브라우저 후속 업무');
    await page.getByLabel('액션 담당자', {exact: true}).fill('브라우저 담당');
    await page.getByRole('button', {name: '액션 저장', exact: true}).click();
    await expect(page.getByRole('region', {name: '수동 액션 만들기'})).toHaveCount(0);
    await expect(page.getByRole('heading', {name: '브라우저 후속 업무', exact: true})).toBeVisible();
    await page.getByLabel('액션 검색', {exact: true}).fill('브라우저 담당');
    await expect(page.locator('.action-item')).toHaveCount(1);
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/actions-desktop.png', fullPage: true, animations: 'disabled'});
    await page.setViewportSize({width: 390, height: 844});
    await expect.poll(() => page.locator('.sidebar').evaluate(element => element.getBoundingClientRect().right)).toBeLessThanOrEqual(0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'mobile action center has no horizontal overflow');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/actions-mobile.png', fullPage: true, animations: 'disabled'});
    await page.setViewportSize({width: 1520, height: 1050});
    await page.getByRole('button', {name: '필터 초기화', exact: true}).click();
    await page.keyboard.press('Control+k');
    await page.getByRole('option', {name: '액션 센터', exact: true}).click();
    await page.getByRole('heading', {name: '액션 센터', exact: true}).waitFor();
    await expect(page.locator('body')).toContainText('회의 자료 검토');
    await page.getByRole('button', {name: '승인', exact: true}).first().click();
    await expect(page.locator('.action-item').first()).toContainText('승인됨');
    await page.goto(base + '/app/team?message=' + process.env.POSTRA_TEST_MESSAGE_ID);
    await page.getByRole('heading', {name: '처리 관리', exact: true}).waitFor();
    await page.locator('.work-detail select').selectOption('in_progress');
    await page.getByLabel('내부 메모', {exact: true}).fill('브라우저 테스트에서 자료 검토를 시작했습니다.');
    await page.getByRole('button', {name: '메모 추가', exact: true}).click();
    await expect(page.locator('.notes-list')).toContainText('브라우저 테스트에서');
    await page.getByLabel('처리 기한', {exact: false}).fill('2050-01-02T12:00');
    await page.getByRole('button', {name: '기한 저장 (빈 값은 해제)', exact: true}).click();
    await expect(page.getByRole('group', {name: '업무 기한 필터'}).getByRole('button', {name: /예정/})).toContainText('1');
    await page.getByRole('group', {name: '업무 기한 필터'}).getByRole('button', {name: /예정/}).click();
    await page.getByRole('combobox', {name: '업무 정렬', exact: true}).selectOption('deadline');
    await expect(page.getByRole('region', {name: '업무 상태 보드'})).toContainText('프로젝트 킥오프 일정 공유');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/work-desktop.png', fullPage: true, animations: 'disabled'});
    await page.setViewportSize({width: 390, height: 844});
    await expect.poll(() => page.locator('.sidebar').evaluate(element => element.getBoundingClientRect().right)).toBeLessThanOrEqual(0);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'mobile work board has no horizontal overflow');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/work-mobile.png', fullPage: true, animations: 'disabled'});
    await page.setViewportSize({width: 1520, height: 1050});
    await page.goto(base + '/app/accounts');
    await page.getByRole('heading', {name: /메일 계정/}).first().waitFor();
    await expect(page.locator('body')).toContainText(process.env.POSTRA_TEST_EMAIL);
    await page.goto(base + '/app/ask');
    await page.getByRole('textbox', {name: 'AI에게 질문'}).fill('회의 자료에 대해 회신할 내용은?');
    await page.getByRole('button', {name: '질문하기', exact: true}).click();
    await expect(page.locator('.ai-result')).toContainText('공유된 회의 자료를 검토');
    await page.goto(base + '/app/digest');
    await page.getByRole('button', {name: '브리핑 생성', exact: true}).click();
    await expect(page.locator('.ai-result')).toContainText('프로젝트 회의 준비');
    await page.goto(base + '/app/compose');
    await page.getByRole('textbox', {name: '받는 사람', exact: true}).fill('team@corp.local');
    await page.getByRole('textbox', {name: '메일 제목', exact: true}).fill('React에서 작성한 HTML 업무 메일');
    await page.getByRole('combobox', {name: '본문 작성 방식', exact: true}).selectOption('html');
    const editor = page.getByRole('textbox', {name: '메일 본문 서식 편집기', exact: true});
    await editor.fill('안녕하세요. 자료 검토를 완료했습니다.\n자세한 내용은 아래를 확인해 주세요.');
    await editor.press('Control+a');
    await page.getByRole('button', {name: '굵게', exact: true}).click();
    await page.getByRole('combobox', {name: '메일 템플릿', exact: true}).selectOption('notice');
    await page.getByRole('button', {name: '서식 적용', exact: true}).click();
    await expect(editor).toContainText('자료 검토를 완료');
    await page.getByRole('button', {name: '초안 저장', exact: true}).click();
    await page.waitForURL(/\/app\/drafts\//);
    await expect(page.locator('.compose-page')).toContainText('저장됨');
    await page.reload();
    await expect(editor).toContainText('자료 검토를 완료');
    await page.getByRole('button', {name: '미리보기 · 발송', exact: true}).click();
    await page.getByRole('heading', {name: '발송 전 최종 확인'}).waitFor();
    const preview = page.frameLocator('iframe[title="서식 메일 미리보기"]');
    await expect(preview.locator('body')).toContainText('자료 검토를 완료');
    assert.equal(await page.locator('iframe[title="서식 메일 미리보기"]').getAttribute('sandbox'), '');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/compose-preview.png', fullPage: true});
    await page.getByRole('button', {name: '내용 확인 · 승인', exact: true}).click();
    await page.getByRole('button', {name: '승인한 메일 발송', exact: true}).click();
    await page.getByRole('heading', {name: '메일을 발송했습니다', exact: true}).waitFor();
    await page.getByRole('link', {name: '발송 기록', exact: true}).click();
    await expect(page.locator('.outbound-item')).toContainText('React에서 작성한 HTML 업무 메일');
    await expect(page.locator('.outbound-item')).toContainText('발송 완료');
    await page.goto(base + '/app/mail');
    await page.setViewportSize({width: 390, height: 844});
    await page.getByRole('heading', {name: '받은메일', exact: true}).waitFor();
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, 'mobile has no horizontal overflow');
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/inbox-mobile.png', fullPage: true});
    // The cookie jar is shared across tabs: switching identity elsewhere must
    // replace both the principal and all mailbox caches in the original tab.
    const otherTab = await page.context().newPage();
    otherTab.on('pageerror', error => errors.push(error.message));
    await otherTab.goto(base + '/app/');
    await otherTab.getByRole('button', {name: '로그아웃', exact: true}).click();
    await otherTab.waitForURL(url => url.searchParams.get('sso') === 'signed_out');
    await otherTab.locator('[name=login_id]').fill(process.env.POSTRA_TEST_OTHER_LOGIN);
    await otherTab.locator('[name=password]').fill(process.env.POSTRA_TEST_OTHER_PASSWORD);
    await otherTab.getByRole('button', {name: '계정으로 로그인', exact: true}).click();
    await otherTab.waitForURL(/\/app\/mail/);
    await expect(otherTab.locator('.mail-list')).toContainText(process.env.POSTRA_TEST_PRIVATE_SUBJECT);
    await expect(page.locator('.mail-list')).toContainText(process.env.POSTRA_TEST_PRIVATE_SUBJECT);
    await expect(page.locator('body')).not.toContainText('프로젝트 킥오프 일정 공유');
    await expect(page.getByRole('link', {name: '관리자 콘솔'})).toHaveCount(0);
    await otherTab.close();
    await page.getByRole('button', {name: '메뉴 열기'}).click();
    await page.getByRole('button', {name: '로그아웃', exact: true}).click();
    await page.waitForURL(url => url.searchParams.get('sso') === 'signed_out');
    await page.getByRole('button', {name: '계정으로 로그인'}).waitFor();
    await expect(page.locator('body')).not.toContainText('프로젝트 킥오프 일정 공유');
    assert.deepEqual(errors, [], 'no browser runtime errors');
    assert.deepEqual(external, [], 'offline runtime must not request external resources');
    console.log('PASS: real login/session/CSRF/privacy/cross-tab identity isolation, inbox/AI, persisted live typography/320px mobile, read-only reply source, dark/command, action/team, accounts/Q&A/digest, rich save/approval/send/logout; no external runtime requests');
  } catch (error) {
    if (process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path: process.env.POSTRA_SPA_SCREENSHOT_DIR + '/failure.png', fullPage: true});
    console.error('Visible errors:', await page.getByRole('alert').allTextContents());
    throw error;
  } finally {await browser.close();}
})().catch(error => {console.error(error); process.exitCode = 1;});

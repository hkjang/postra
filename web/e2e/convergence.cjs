// Optional browser gate driven by TestSPAConvergenceBrowser. Every server,
// issuer, image and identity is a disposable localhost test fixture.
const assert = require('node:assert/strict');
const {chromium, expect} = require('@playwright/test');

(async()=>{
  const base=process.env.POSTRA_TEST_URL;
  const origins=[base,process.env.POSTRA_TEST_OIDC_URL,process.env.POSTRA_TEST_IMAGE_URL];
  const browser=await chromium.launch({headless:true,...(process.env.POSTRA_CHROME?{executablePath:process.env.POSTRA_CHROME}:{})});
  const context=await browser.newContext({viewport:{width:1440,height:1000}});
  const page=await context.newPage(),errors=[],external=[];
  page.setDefaultTimeout(15000);
  let imageRequests=0;
  context.on('request',request=>{
    if(request.url().startsWith(process.env.POSTRA_TEST_IMAGE_URL)) imageRequests++;
    if(!origins.some(origin=>request.url().startsWith(origin+'/'))&&!/^(about|data|blob):/.test(request.url())) external.push(request.url());
  });
  page.on('pageerror',error=>errors.push(error.message));
  page.on('dialog',dialog=>dialog.type()==='beforeunload'?dialog.accept():dialog.dismiss());
  const login=async(target,who,password)=>{
    await target.getByLabel('로그인 ID',{exact:true}).fill(who);
    await target.getByLabel('비밀번호',{exact:true}).fill(password);
    await target.getByRole('button',{name:'계정으로 로그인',exact:true}).click();
  };
  const logout=async(target)=>{
    await target.getByRole('button',{name:'로그아웃',exact:true}).click();
    await target.waitForURL(url=>url.searchParams.get('sso')==='signed_out');
    await expect(target.getByRole('button',{name:'계정으로 로그인',exact:true})).toBeVisible();
  };
  const save=async()=>{
    await page.getByRole('button',{name:'변경 확인',exact:true}).click();
    await page.getByRole('dialog').getByRole('button',{name:'확인 후 저장',exact:true}).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await expect(page.locator('.settings-savebar')).toContainText('모든 변경사항이 저장');
  };
  const viewportCheck=async()=>{
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'mobile page must not overflow horizontally');
    await expect(page.locator('h1')).toHaveCount(1);
    const unnamed=await page.locator('button:visible').evaluateAll(buttons=>buttons.filter(button=>!button.innerText.trim()&&!button.getAttribute('aria-label')&&!button.getAttribute('title')).length);
    assert.equal(unnamed,0,'visible icon buttons must have an accessible name');
  };
  try {
    await page.goto(base+'/app/');
    await page.getByRole('link',{name:'최초 관리자 설정',exact:true}).click();
    await expect(page.getByRole('heading',{name:'최초 관리자 설정',exact:true})).toBeVisible();
    await page.getByLabel('로그인 ID',{exact:true}).fill(process.env.POSTRA_TEST_LOGIN);
    await page.getByLabel('표시 이름',{exact:true}).fill('정책 관리자');
    await page.getByLabel('비밀번호',{exact:true}).fill(process.env.POSTRA_TEST_PASSWORD);
    await page.getByLabel('비밀번호 확인',{exact:true}).fill(process.env.POSTRA_TEST_PASSWORD);
    await page.getByRole('button',{name:'관리자 계정 만들기',exact:true}).click();
    await expect(page.getByRole('heading',{name:'운영 콘솔',exact:true})).toBeVisible();
    assert.doesNotMatch(await page.evaluate(()=>document.cookie),/postra_session=/,'session cookie must remain HttpOnly');
    await page.getByRole('textbox',{name:'관리자 설정 검색'}).fill('ui.theme');
    const theme=page.locator('[id="setting-ui.theme"]');
    await theme.selectOption('dark');
    await page.locator('.setting-row').filter({has:theme}).getByLabel('관리자 값 강제 적용').check();
    await save();
    await expect(page.locator('html')).toHaveAttribute('data-theme','dark');
    await page.getByRole('textbox',{name:'관리자 설정 검색'}).fill('mail.external_images');
    await page.locator('[id="setting-mail.external_images"]').selectOption('allow_once');
    await save();
    await page.goto(base+'/app/settings');
    await page.getByRole('textbox',{name:'설정 검색'}).fill('ui.theme');
    await expect(page.locator('[id="setting-ui.theme"]')).toBeDisabled();
    await expect(page.locator('[id="setting-ui.theme"]')).toHaveValue('dark');
    await expect(page.locator('.setting-row')).toContainText('조직 정책');
    await page.goto(base+'/app/messages/convergence-own');
    await expect(page.getByRole('heading',{name:'정책 확인용 내 메일'})).toBeVisible();
    await expect(page.frameLocator('iframe[title="메일 본문"]').locator('body')).toContainText('안전한 본문');
    assert.equal(imageRequests,0,'opening a received message must not request remote images');
    await page.getByRole('button',{name:'이번만 이미지 표시',exact:true}).click();
    const received=page.frameLocator('iframe[title="메일 본문"]');
    await expect(received.locator('img[src]')).toHaveCount(1);
    await expect.poll(()=>imageRequests).toBe(1);
    await page.reload();
    await expect(page.getByRole('button',{name:'이번만 이미지 표시'})).toBeVisible();
    assert.equal(imageRequests,1,'one-time consent must not persist across reload');
    const privateCode=await page.evaluate(async()=>(await fetch('/api/messages/convergence-private')).status);
    assert.equal(privateCode,404,'admin cannot read another mailbox');
    const csrfCode=await page.evaluate(async()=>(await fetch('/api/messages/convergence-own/images/allow',{method:'POST',headers:{'Content-Type':'application/json'},body:'{"scope":"sender"}'})).status);
    assert.equal(csrfCode,403,'browser writes require CSRF');
    await page.setViewportSize({width:390,height:844});
    for(const path of ['/app/mail','/app/drafts','/app/jobs','/app/settings']){
      await page.goto(base+path);await expect(page.locator('h1')).toBeVisible();await viewportCheck();
    }
    await page.setViewportSize({width:1440,height:1000});
    await page.goto(base+'/app/mail');
    await expect(page.locator('.mail-list')).toContainText('정책 확인용 내 메일');
    const member=await context.newPage();member.on('pageerror',error=>errors.push(error.message));
    await member.goto(base+'/app/mail');
    await logout(member);
    await login(member,process.env.POSTRA_TEST_OTHER_LOGIN,process.env.POSTRA_TEST_OTHER_PASSWORD);
    await expect(member.locator('.mail-list')).toContainText('개인 사용자 비밀 메일');
    await expect(page.locator('.mail-list')).toContainText('개인 사용자 비밀 메일');
    await expect(page.locator('body')).not.toContainText('정책 확인용 내 메일');
    await expect(member.locator('html')).toHaveAttribute('data-theme','dark');
    await member.goto(base+'/app/settings');
    await member.getByRole('textbox',{name:'설정 검색'}).fill('ui.theme');
    await expect(member.locator('[id="setting-ui.theme"]')).toBeDisabled();
    const noAdmin=await member.evaluate(async()=>(await fetch('/api/admin/configuration')).status);
    assert.equal(noAdmin,403,'member cannot query administrator secrets or settings');
    await logout(member);
    await member.close();
    await page.goto(base+'/app/login');
    await login(page,process.env.POSTRA_TEST_LOGIN,process.env.POSTRA_TEST_PASSWORD);
    await expect(page.locator('.mail-list')).toContainText('정책 확인용 내 메일');
    await logout(page);
    await page.goto(base+'/auth/oidc/start?return_to=%2Fapp%2Fsettings');
    await page.waitForURL(base+'/app/settings');
    await expect(page.getByRole('heading',{name:'개인 설정',exact:true})).toBeVisible();
    const sso=await page.evaluate(async()=>(await fetch('/auth/session')).json());
    assert.equal(sso.principal.auth_method,'oidc','new callback must establish a real verified OIDC session');
    assert.equal(sso.principal.login_id,'oidc-browser');
    assert.doesNotMatch(page.url(),/[?&](code|state)=/,'callback must strip provider credentials');
    await logout(page);
    await expect(page.getByRole('button',{name:'계정으로 로그인',exact:true})).toBeVisible();
    await page.setViewportSize({width:390,height:844});await viewportCheck();
    const storage=await page.evaluate(()=>({local:{...localStorage},session:{...sessionStorage}}));
    assert.doesNotMatch(JSON.stringify(storage),new RegExp(process.env.POSTRA_TEST_PASSWORD));
    assert.deepEqual(errors,[],'no runtime exceptions');assert.deepEqual(external,[],'no external production service requests');
    console.log('PASS: standalone setup/local login/logout, real signed OIDC callback/deep return, admin settings save/forced personal policy, two-user cross-tab privacy, received-image opt-in/CSP/reload, mobile headings/controls/overflow');
  } catch(error) {
    console.error('Browser phase failed:',error);
    try {
      if(process.env.POSTRA_SPA_SCREENSHOT_DIR) await page.screenshot({path:process.env.POSTRA_SPA_SCREENSHOT_DIR+'/convergence-failure.png',fullPage:true});
      console.error('Visible errors:',await page.getByRole('alert').allTextContents());
    } catch { /* a navigation must never obscure the original assertion */ }
    throw error;
  } finally {await browser.close();}
})().catch(error=>{console.error(error);process.exitCode=1});

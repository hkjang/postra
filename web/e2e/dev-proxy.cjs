// Real Vite -> Go proxy regression: no auth/CSRF bypass or rewritten Host.
const assert=require('node:assert/strict');
const path=require('node:path');
const {chromium,expect}=require('@playwright/test');
(async()=>{
  const {createServer}=await import('vite');
  const vite=await createServer({root:path.resolve(__dirname,'..'),logLevel:'error',server:{host:'127.0.0.1',port:0}});
  await vite.listen();
  const base='http://127.0.0.1:'+vite.httpServer.address().port;
  const browser=await chromium.launch({headless:true,...(process.env.POSTRA_CHROME?{executablePath:process.env.POSTRA_CHROME}:{})});
  const page=await browser.newPage();const errors=[];page.on('pageerror',error=>errors.push(error.message));
  try{
    await page.goto(base+'/app/login');
    await page.getByLabel('로그인 ID',{exact:true}).fill(process.env.POSTRA_TEST_LOGIN);
    await page.getByLabel('비밀번호',{exact:true}).fill(process.env.POSTRA_TEST_PASSWORD);
    await page.getByRole('button',{name:'계정으로 로그인',exact:true}).click();
    await page.waitForURL(base+'/app/mail');
    await expect(page.locator('.mail-list')).toContainText('개발 프록시 메일');
    await page.goto(base+'/app/messages/dev-proxy-message');
    await expect(page.frameLocator('iframe[title="메일 본문"]').locator('body')).toContainText('Vite 인증된 본문');
    assert.doesNotMatch(await page.evaluate(()=>document.cookie),/postra_session=/);
    assert.equal(await page.evaluate(async()=>(await fetch('/api/messages/batch',{method:'POST',headers:{'Content-Type':'application/json'},body:'{"message_ids":["dev-proxy-message"],"action":"mark_unread"}'})).status),403);
    await expect(page.getByRole('button',{name:'안읽음 표시',exact:true})).toBeVisible();
    await page.getByRole('button',{name:'로그아웃',exact:true}).click();
    await page.waitForURL(url=>url.searchParams.get('sso')==='signed_out');
    await expect(page.getByRole('button',{name:'계정으로 로그인',exact:true})).toBeVisible();
    assert.deepEqual(errors,[]);
    console.log('PASS: real Vite /auth login/logout, same-origin CSRF, /api mailbox and authenticated received-HTML iframe');
  }finally{await browser.close();await vite.close()}
})().catch(error=>{console.error(error);process.exitCode=1});

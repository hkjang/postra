// Optional renderer for build_docs.py; reuses existing web development tools.
// Local guide contents and fonts never require external network access.
const path = require('node:path');
const {chromium} = require(require.resolve('@playwright/test', {paths: [path.resolve(__dirname, '../web')]}));

(async () => {
  const [executablePath, documentURL, pdfPath] = process.argv.slice(2);
  if (!executablePath || !documentURL?.startsWith('file:') || !pdfPath) throw new Error('Local browser, document and PDF paths are required');
  const browser = await chromium.launch({headless: true, executablePath});
  try {
    const context = await browser.newContext();
    await context.route('**/*', route => /^(?:file|data):/.test(route.request().url()) ? route.continue() : route.abort());
    const page = await context.newPage();
    await page.goto(documentURL, {waitUntil: 'load'});
    await page.evaluate(() => document.fonts.ready);
    await page.pdf({path: pdfPath, preferCSSPageSize: true, printBackground: true, displayHeaderFooter: false});
  } finally {
    await browser.close();
  }
})().catch(error => {console.error(error.message); process.exitCode = 1;});

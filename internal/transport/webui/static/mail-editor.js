/* Self-hosted, dependency-free editor: works on disconnected networks. */
(() => {
  'use strict';
  const allowedTags = new Set('p div span br hr h1 h2 h3 h4 h5 h6 strong b em i u s strike blockquote pre code ul ol li table thead tbody tfoot tr td th a'.split(' '));
  const droppedTags = new Set('script style iframe object embed svg math form input button textarea select link meta base img video audio source'.split(' '));
  const allowedStyles = new Set('color background-color font-family font-size font-weight font-style text-decoration text-align line-height padding padding-top padding-right padding-bottom padding-left margin margin-top margin-right margin-bottom margin-left border border-top border-right border-bottom border-left border-collapse border-radius width max-width vertical-align'.split(' '));
  function safeURL(value) {
    const url = value.trim();
    if (/[\u0000-\u0020\u007f]/.test(url)) return '';
    try {
      const parsed = new URL(url);
      return ['https:', 'http:', 'mailto:'].includes(parsed.protocol) ? url : '';
    } catch (_) { return ''; }
  }
  // Rebuild a small set of passive elements before inserting pasted/source HTML.
  // The server independently applies the authoritative sanitization policy.
  function cleanHTML(raw) {
    const doc = new DOMParser().parseFromString(raw, 'text/html');
    const result = document.createElement('div');
    function copy(node, parent) {
      if (node.nodeType === Node.TEXT_NODE) {
        parent.appendChild(document.createTextNode(node.nodeValue));
        return;
      }
      if (node.nodeType !== Node.ELEMENT_NODE) return;
      const tag = node.tagName.toLowerCase();
      if (droppedTags.has(tag)) return;
      if (!allowedTags.has(tag) && tag !== 'font') {
        Array.from(node.childNodes).forEach(child => copy(child, parent));
        return;
      }
      const el = document.createElement(tag === 'font' ? 'span' : tag);
      for (const prop of allowedStyles) {
        const value = node.style.getPropertyValue(prop);
        if (value && /^[#(),.%\w\s+\-"']+$/.test(value) && !/url|expression|var\s*\(/i.test(value)) el.style.setProperty(prop, value);
      }
      if (tag === 'font' && /^#[a-f\d]{3,8}$/i.test(node.getAttribute('color') || '')) el.style.color = node.getAttribute('color');
      if (tag === 'a') {
        const href = safeURL(node.getAttribute('href') || '');
        if (href) el.setAttribute('href', href);
        el.setAttribute('rel', 'noreferrer noopener');
      }
      if (tag === 'td' || tag === 'th') {
        for (const attr of ['colspan', 'rowspan']) {
          const value = node.getAttribute(attr);
          if (/^[1-9]\d?$/.test(value || '')) el.setAttribute(attr, value);
        }
      }
      Array.from(node.childNodes).forEach(child => copy(child, el));
      parent.appendChild(el);
    }
    Array.from(doc.body.childNodes).forEach(node => copy(node, result));
    return result.innerHTML;
  }
  function plainHTML(value) {
    const div = document.createElement('div');
    for (const line of value.split('\n')) {
      const p = document.createElement('p');
      p.textContent = line;
      if (!line) p.appendChild(document.createElement('br'));
      div.appendChild(p);
    }
    return div.innerHTML;
  }
  function plainText(html) {
    const doc = new DOMParser().parseFromString(html, 'text/html');
    let text = '';
    function walk(node) {
      if (node.nodeType === Node.TEXT_NODE) { text += node.nodeValue; return; }
      const block = /^(P|DIV|H[1-6]|LI|BLOCKQUOTE|PRE|TR|UL|OL|TABLE)$/.test(node.nodeName);
      if (block && text && !text.endsWith('\n')) text += '\n';
      if (node.nodeName === 'LI') text += '• ';
      if (node.nodeName === 'BR') text += '\n';
      Array.from(node.childNodes).forEach(walk);
      if (node.nodeName === 'A' && node.getAttribute('href') && node.textContent !== node.getAttribute('href')) text += ' (' + node.getAttribute('href') + ')';
      if (node.nodeName === 'TD' || node.nodeName === 'TH') text += '\t';
      if (block && !text.endsWith('\n')) text += '\n';
    }
    walk(doc.body);
    return text.replace(/\u00a0/g, ' ').replace(/\n{3,}/g, '\n\n').trim();
  }
  document.querySelectorAll('[data-mail-editor]').forEach(root => {
    const form = root.closest('form');
    const modeSelect = root.querySelector('[data-editor-mode]');
    const format = root.querySelector('[data-body-format]');
    const source = root.querySelector('[data-html-source]');
    const plain = root.querySelector('[data-plain-source]');
    const frame = root.querySelector('[data-editor-frame]');
    const tools = root.querySelector('[data-editor-tools]');
    const themes = root.querySelector('[data-editor-themes]');
    const sourcePanel = root.querySelector('[data-source-panel]');
    const linkPanel = root.querySelector('[data-link-panel]');
    const linkURL = root.querySelector('[data-link-url]');
    const status = root.querySelector('[data-editor-status]');
    let mode = format.value;
    let body, doc, savedRange, templateContent;
    const initialHTML = cleanHTML(source.value || plainHTML(plain.value));
    function rememberSelection() {
      const selection = frame.contentWindow.getSelection();
      if (selection.rangeCount && body.contains(selection.anchorNode)) savedRange = selection.getRangeAt(0).cloneRange();
    }
    function restoreSelection() {
      body.focus();
      if (savedRange && body.contains(savedRange.commonAncestorContainer)) {
        const selection = frame.contentWindow.getSelection();
        selection.removeAllRanges();
        selection.addRange(savedRange);
      }
    }
    function sync() {
      if (mode === 'plain') { format.value = 'plain'; source.value = ''; return; }
      format.value = 'html';
      source.value = cleanHTML(mode === 'source' ? source.value : body.innerHTML);
      plain.value = plainText(source.value);
    }
    function showMode() {
      modeSelect.value = mode;
      frame.hidden = mode !== 'html';
      tools.hidden = mode !== 'html';
      themes.hidden = mode !== 'html';
      plain.hidden = mode !== 'plain';
      sourcePanel.hidden = mode !== 'source';
      linkPanel.hidden = true;
    }
    function command(name, value) {
      restoreSelection();
      doc.execCommand('styleWithCSS', false, true);
      doc.execCommand(name, false, value || null);
      rememberSelection();
      sync();
    }
    frame.addEventListener('load', () => {
      doc = frame.contentDocument;
      body = doc.body;
      body.innerHTML = initialHTML;
      body.contentEditable = 'true';
      body.setAttribute('role', 'textbox');
      body.setAttribute('aria-label', '메일 본문');
      body.setAttribute('aria-multiline', 'true');
      doc.addEventListener('selectionchange', rememberSelection);
      body.addEventListener('input', sync);
      body.addEventListener('click', event => { if (event.target.closest('a')) event.preventDefault(); });
      body.addEventListener('dragover', event => event.preventDefault());
      body.addEventListener('drop', event => event.preventDefault());
      body.addEventListener('paste', event => {
        if (!event.clipboardData) return;
        event.preventDefault();
        const html = event.clipboardData.getData('text/html');
        command('insertHTML', html ? cleanHTML(html) : plainHTML(event.clipboardData.getData('text/plain')));
      });
      showMode();
      root.dataset.ready = 'true';
    }, {once: true});
    frame.srcdoc = '<!doctype html><html lang="ko"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src \'none\'; style-src \'unsafe-inline\'; base-uri \'none\'; form-action \'none\'"><style>body{box-sizing:border-box;min-height:380px;margin:0;padding:22px;color:#172033;background:#fff;font:15px/1.65 Arial,sans-serif;overflow-wrap:anywhere;outline:none}p{margin:0 0 12px}table{max-width:100%}a{color:#3157d5}blockquote{border-left:3px solid #dfe5ee;margin-left:0;padding-left:16px}pre{white-space:pre-wrap}</style></head><body></body></html>';
    modeSelect.addEventListener('change', () => {
      if (!body) return;
      const next = modeSelect.value;
      sync();
      if (mode === 'plain' && next !== 'plain') source.value = plainHTML(plain.value);
      if (next === 'html') {
        body.innerHTML = cleanHTML(source.value);
        savedRange = null;
        templateContent = null;
      }
      mode = next;
      sync();
      showMode();
      status.textContent = mode === 'plain' ? '일반 텍스트로 저장하면 기존 HTML 서식은 제거됩니다.' : '';
    });
    tools.addEventListener('mousedown', event => { if (event.target.closest('button')) event.preventDefault(); });
    tools.querySelectorAll('[data-command]').forEach(button => button.addEventListener('click', () => command(button.dataset.command)));
    root.querySelector('[data-block]').addEventListener('change', event => command('formatBlock', event.target.value));
    root.querySelector('[data-color]').addEventListener('input', event => command('foreColor', event.target.value));
    root.querySelector('[data-show-link]').addEventListener('click', () => {
      rememberSelection();
      linkPanel.hidden = false;
      linkURL.focus();
    });
    root.querySelector('[data-close-link]').addEventListener('click', () => { linkPanel.hidden = true; restoreSelection(); });
    function insertLink() {
      const href = safeURL(linkURL.value);
      if (!href) { status.textContent = 'http://, https:// 또는 mailto:로 시작하는 올바른 주소를 입력하세요.'; return; }
      restoreSelection();
      if (frame.contentWindow.getSelection().isCollapsed) {
        const anchor = document.createElement('a');
        anchor.href = href;
        anchor.textContent = href;
        command('insertHTML', anchor.outerHTML);
      } else command('createLink', href);
      linkPanel.hidden = true;
      linkURL.value = '';
      status.textContent = '';
    }
    root.querySelector('[data-insert-link]').addEventListener('click', insertLink);
    linkURL.addEventListener('keydown', event => { if (event.key === 'Enter') { event.preventDefault(); insertLink(); } });
    themes.querySelectorAll('[data-theme]').forEach(button => button.addEventListener('click', () => {
      const content = cleanHTML(templateContent && body.contains(templateContent) ? templateContent.innerHTML : body.innerHTML);
      const theme = button.dataset.theme;
      const accent = theme === 'report' ? '#137568' : '#3157d5';
      const heading = theme === 'notice' ? '안내드립니다' : '업무 공유';
      body.innerHTML = '<table style="width:100%;border-collapse:collapse;background-color:#f1f4f9"><tbody><tr><td style="padding:24px"><table style="width:100%;max-width:640px;border-collapse:collapse;background-color:#ffffff"><tbody>' +
        (theme === 'letter' ? '' : '<tr><td style="padding:24px;background-color:' + accent + ';color:#ffffff;font-family:Arial,sans-serif"><h2 style="margin:0;font-size:24px;color:#ffffff">' + heading + '</h2></td></tr>') +
        '<tr><td style="padding:24px;font-family:Arial,sans-serif;font-size:15px;line-height:1.6;color:#172033">' + (content || '<p><br></p>') + '</td></tr></tbody></table></td></tr></tbody></table>';
      templateContent = body.querySelector('table table tbody').lastElementChild.firstElementChild;
      savedRange = null;
      sync();
      status.textContent = '기존 본문을 유지하고 디자인을 적용했습니다. 제목과 내용을 자유롭게 수정하세요.';
    }));
    form.addEventListener('submit', () => {
      if (body) sync();
      // The optional link input is not part of the mail settings.
      linkURL.value = '';
    });
    // Avoid accidental loss when navigating to preview before saving edits.
    let dirty = false;
    form.addEventListener('input', () => { dirty = true; });
    frame.addEventListener('load', () => frame.contentDocument.addEventListener('input', () => { dirty = true; }), {once: true});
    root.addEventListener('click', event => { if (event.target.closest('[data-command],[data-theme],[data-insert-link]')) dirty = true; });
    form.addEventListener('submit', () => { dirty = false; });
    window.addEventListener('beforeunload', event => { if (dirty) { event.preventDefault(); event.returnValue = ''; } });
  });
})();

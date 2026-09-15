import { Extension, Node, mergeAttributes } from '@tiptap/react'

// Preserve only static email presentation. Source-mode HTML is untrusted even
// before its first save: CSS network functions must never reach the live editor.
export function safeMailStyle(raw: string | null): string {
  if (!raw) return ''
  const scratch = document.createElement('span')
  scratch.style.cssText = raw
  const safe = document.createElement('span')
  const allowed = /^(color|background-color|font-family|font-size|font-weight|font-style|text-decoration|text-align|vertical-align|line-height|letter-spacing|padding(-top|-right|-bottom|-left)?|margin(-top|-right|-bottom|-left)?|border(-top|-right|-bottom|-left|-color|-style|-width|-radius|-collapse|-spacing)?|width|max-width|height|list-style-type|white-space)$/
  for (const property of Array.from(scratch.style)) {
    const value = scratch.style.getPropertyValue(property)
    if (allowed.test(property) && !/[\\@]|url\s*\(|expression\s*\(|var\s*\(|image-set\s*\(/i.test(value)) safe.style.setProperty(property, value)
  }
  return safe.style.cssText
}

export const MailContainer = Node.create({
  name: 'mailContainer', group: 'block', content: 'block+', defining: true,
  addAttributes() {
    return {
      template: { default: null, parseHTML: element => { const value = element.getAttribute('data-postra-template'); return /^(clean|formal|concise|notice|report|newsletter)$/.test(value || '') ? value : null }, renderHTML: attrs => attrs.template ? { 'data-postra-template': attrs.template } : {} },
      signature: { default: null, parseHTML: element => { const value = element.getAttribute('data-postra-signature'); return /^sig_[A-Za-z0-9_-]{1,120}$/.test(value || '') ? value : null }, renderHTML: attrs => attrs.signature ? { 'data-postra-signature': attrs.signature } : {} },
    }
  },
  parseHTML() { return [{ tag: 'div[data-postra-template]' }, { tag: 'div[data-postra-signature]' }] },
  renderHTML({ HTMLAttributes }) { return ['div', mergeAttributes(HTMLAttributes), 0] },
})

export const MailStyles = Extension.create({
  name: 'mailStyles',
  addGlobalAttributes() {
    return [{ types: ['mailContainer', 'paragraph', 'heading', 'blockquote', 'bulletList', 'orderedList', 'listItem', 'table', 'tableCell', 'tableHeader', 'codeBlock', 'horizontalRule'], attributes: {
      mailStyle: { default: null, parseHTML: element => safeMailStyle(element.getAttribute('style')), renderHTML: attributes => attributes.mailStyle ? { style: safeMailStyle(attributes.mailStyle) } : {} },
    } }]
  },
})

export const MailImage = Node.create({
  name: 'mailImage', inline: true, group: 'inline', atom: true,
  addAttributes() { return { src: { default: '' }, alt: { default: '' } } },
  parseHTML() { return [{ tag: 'img', getAttrs: element => { const src = element.getAttribute('src') || ''; let allowed = /^cid:postra-att_[A-Za-z0-9_-]{1,120}@postra\.local$/.test(src); try { const url = new URL(src); allowed ||= (url.protocol === 'https:' || url.protocol === 'http:') && !!url.hostname && !url.username && !url.password } catch { /* Not an approved URL. */ } return allowed ? { src, alt: element.getAttribute('alt') || '' } : false } }] },
  renderHTML({ HTMLAttributes }) { return ['img', mergeAttributes({ style: 'max-width:100%;height:auto' }, HTMLAttributes)] },
  // The editing DOM does not resolve arbitrary CID URLs. The final preview
  // loads only this user's scanned attachments through the authenticated API.
  addNodeView() { return ({ node }) => { const dom = document.createElement('span'); dom.className = 'compose-inline-placeholder'; dom.textContent = `🖼 ${node.attrs.alt || '본문 이미지'} · 미리보기에서 확인`; return { dom } } },
})

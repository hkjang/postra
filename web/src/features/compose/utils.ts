export function textToHTML(text: string): string {
  const escaped = text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
  return escaped.split(/\r?\n/).map(line => `<p>${line || '<br>'}</p>`).join('')
}

// Do not split a quoted display name ("Hong, Gildong" <hong@corp.local>).
export function splitRecipients(value: string): string[] {
  const result: string[] = []
  let start = 0, quoted = false, escaped = false, brackets = 0
  for (let i = 0; i < value.length; i++) {
    const c = value[i]
    if (escaped) { escaped = false; continue }
    if (quoted && c === '\\') { escaped = true; continue }
    if (c === '"') quoted = !quoted
    if (!quoted && c === '<') brackets++
    if (!quoted && c === '>') brackets = Math.max(0, brackets - 1)
    if (!quoted && !brackets && /[,;\n]/.test(c)) { const address = value.slice(start, i).trim(); if (address) result.push(address); start = i + 1 }
  }
  const last = value.slice(start).trim()
  if (last) result.push(last)
  return result
}

export function safeLink(value: string): boolean {
  if (/[\u0000-\u0020\u007f]/.test(value)) return false
  try { return ['https:', 'http:', 'mailto:'].includes(new URL(value).protocol) } catch { return false }
}

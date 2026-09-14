import { describe, expect, it } from 'vitest'
import { safeLink, splitRecipients, textToHTML } from './utils'

describe('compose helpers', () => {
  it('keeps display names with commas together and supports semicolons', () => {
    expect(splitRecipients('"Hong, Gildong" <hong@corp.local>, kim@corp.local; lee@corp.local')).toEqual(['"Hong, Gildong" <hong@corp.local>', 'kim@corp.local', 'lee@corp.local'])
  })
  it('clears recipient arrays and preserves escaped display-name quotes', () => {
    expect(splitRecipients(' , ; ')).toEqual([])
    expect(splitRecipients('"Hong \\"Gildong\\"" <hong@corp.local>')).toHaveLength(1)
  })
  it('escapes markup when switching plain text to HTML', () => {
    expect(textToHTML('<img src=x onerror=alert(1)>\nA & B')).toBe('<p>&lt;img src=x onerror=alert(1)&gt;</p><p>A &amp; B</p>')
  })
  it('allows only explicit web and email links', () => {
    expect(safeLink('https://corp.local/doc')).toBe(true)
    expect(safeLink('mailto:hong@corp.local')).toBe(true)
    for (const link of ['javascript:alert(1)', 'data:text/html,test', 'java\nscript:alert(1)', '//external.test', '/api/admin/users']) expect(safeLink(link)).toBe(false)
  })
})

// @vitest-environment node
// CSS source invariants complement real-browser font-size/overflow regressions.
import {readdirSync, readFileSync} from 'node:fs'
import {join} from 'node:path'
import {describe, expect, it} from 'vitest'

const root = join(process.cwd(), 'src')
function files(directory: string): string[] {
  return readdirSync(directory, {withFileTypes: true}).flatMap(entry => entry.isDirectory() ? files(join(directory, entry.name)) : entry.name.endsWith('.css') ? [join(directory, entry.name)] : [])
}
function rules(path: string) {
  const css = readFileSync(path, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '')
  return Array.from(css.matchAll(/([^{}]+)\{([^{}]*)\}/g), match => ({selector: match[1].trim(), body: match[2]}))
}
const typography = readFileSync(join(root, 'typography.css'), 'utf8')
const tokens = new Set(Array.from(typography.matchAll(/(--font-[a-z-]+)\s*:/g), match => match[1]))
const authoredEditor = new Set(['.rich-editor-content', '.rich-editor-content h2', '.rich-editor-content h3'])

describe('semantic workspace typography', () => {
  it('uses rem-based semantic sizes with a readable floor and a single large-text scale', () => {
    for (const [role, rem] of Object.entries({body: '.9375', meta: '.8125', label: '.75', title: '1', section: '1.125', page: '1.5', reading: '1'})) {
      expect(typography).toContain(`--font-${role}: calc(${rem}rem * var(--ui-font-scale))`)
    }
    expect(typography).toContain('--font-control: var(--font-body)')
    expect(typography).toContain(':root[data-text-size=large] { --ui-font-scale: 1.125; }')
    for (const rule of rules(join(root, 'typography.css')).filter(rule => rule.selector.startsWith(':root'))) {
      expect(rule.body).not.toMatch(/(?:^|;)\s*(?:font-size|zoom|transform)\s*:/)
    }
    expect(typography).not.toContain('!important')
    expect(readFileSync(join(root, 'styles.css'), 'utf8')).toContain('@import "./typography.css"')
  })

  it('routes every UI font declaration through a defined semantic token, including mobile overrides', () => {
    const violations: string[] = []
    for (const path of files(root)) for (const rule of rules(path)) {
      // These are authored-mail editor defaults, not workspace chrome. Explicit
      // email text formatting and isolated preview documents must stay intact.
      if (path.endsWith('compose.css') && authoredEditor.has(rule.selector)) continue
      for (const declaration of rule.body.matchAll(/(?:^|;)\s*(font-size|font)\s*:\s*([^;}]*)/g)) {
        const value = declaration[2].trim()
        if (declaration[1] === 'font' && value === 'inherit') continue
        const token = value.match(/^var\((--font-[a-z-]+)(?:,\s*[^)]+)?\)/)?.[1]
        if (!token || !tokens.has(token) || value.includes('!important')) violations.push(`${path}: ${rule.selector}: ${declaration[1]}:${value}`)
      }
    }
    expect(violations).toEqual([])
  })

  it('keeps button variants and form fields at one size while density only changes spacing', () => {
    const shared = rules(join(root, 'styles.css'))
    for (const selector of ['.field', '.input', '.button', '.button-sm', '.nav-item', '.tabs button,.tabs a']) {
      expect(shared.find(rule => rule.selector === selector)?.body).toContain('font-size:var(--font-control)')
    }
    for (const path of files(root)) for (const rule of rules(path)) {
      if (/data-density|\.compact\b/.test(rule.selector)) expect(rule.body).not.toMatch(/(?:^|;)\s*font(?:-size)?\s*:/)
    }
  })

  it('uses a foreground token for settings details and policy captions', () => {
    const settings = rules(join(root, 'features/settings/settings.css'))
    for (const selector of ['.setting-details', '.policy-lock']) {
      expect(settings.find(rule => rule.selector === selector)?.body).toContain('color:var(--muted-foreground)')
    }
  })
})

import { describe, expect, it } from 'vitest'
import { attachmentURL, inlineDocument } from './DraftBody'

describe('owner-loaded inline image previews', () => {
  it('only maps known CID attachments to inert image data and never permits remote loading', () => {
    const result = inlineDocument('<p>본문</p><img src="cid:postra-att_one@postra.local" alt="로고"><img src="https://tracker.test/pixel"><img src="cid:postra-att_other@postra.local">', { 'postra-att_one@postra.local': 'data:image/png;base64,eA==' })
    expect(result).toContain('img-src data:')
    expect(result).toContain('data:image/png;base64,eA==')
    expect(result).not.toContain('tracker.test')
    expect(result).not.toContain('postra-att_other')
    expect(result).toContain("default-src 'none'")
    expect(result).toContain("form-action 'none'")
  })

  it('rejects SVG and remote URLs even if offered as an image mapping', () => {
    expect(inlineDocument('<img src="cid:postra-att_one@postra.local">', { 'postra-att_one@postra.local': 'data:image/svg+xml;base64,eA==' })).not.toContain('<img')
    expect(inlineDocument('<img src="cid:postra-att_one@postra.local">', { 'postra-att_one@postra.local': 'https://tracker.test' })).not.toContain('tracker.test')
    expect(attachmentURL('draft/id', 'att/id', 3)).toBe('/api/drafts/draft%2Fid/attachments/att%2Fid?version=3')
  })
})

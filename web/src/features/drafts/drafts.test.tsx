import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {cleanup, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {MemoryRouter} from 'react-router-dom'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {DraftsPage} from './index'
vi.mock('@/api/client', () => ({api: vi.fn()}))
const mockAPI = vi.mocked(api)
const draft = {id:'draft-a',status:'open',current_version:2,updated_at:1700000000,subject:'서버에 저장된 초안',to:[{email:'to@corp.local'}],author:'user'}
beforeEach(() => mockAPI.mockReset())
afterEach(() => {cleanup(); vi.restoreAllMocks()})
function mount() {render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><MemoryRouter><DraftsPage/></MemoryRouter></QueryClientProvider>)}
it('loads persisted drafts into an empty browser cache and follows server cursors', async () => {
  const user=userEvent.setup()
  mockAPI.mockResolvedValueOnce({drafts:[draft],next_cursor:'cursor-two'}).mockResolvedValueOnce({drafts:[{...draft,id:'draft-b',subject:'두 번째 페이지 초안'}]})
  mount(); await screen.findByText('서버에 저장된 초안')
  await user.click(screen.getByRole('button',{name:'초안 더 보기'}))
  await screen.findByText('두 번째 페이지 초안')
  expect(mockAPI).toHaveBeenNthCalledWith(2,'/api/drafts?status=open&limit=50&cursor=cursor-two',expect.objectContaining({signal:expect.any(AbortSignal)}))
})
it('does not delete without confirmation and invalidates after an explicit discard', async () => {
  const user=userEvent.setup(); const confirm=vi.spyOn(window,'confirm').mockReturnValue(false)
  mockAPI.mockResolvedValue({drafts:[draft]}); mount()
  await user.click(await screen.findByRole('button',{name:'서버에 저장된 초안 초안 삭제'}))
  expect(mockAPI.mock.calls.some(([,options])=>options?.method==='DELETE')).toBe(false)
  confirm.mockReturnValue(true)
  await user.click(screen.getByRole('button',{name:'서버에 저장된 초안 초안 삭제'}))
  await waitFor(()=>expect(mockAPI).toHaveBeenCalledWith('/api/drafts/draft-a',{method:'DELETE'}))
})

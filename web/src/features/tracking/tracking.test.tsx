import {afterEach,beforeEach,expect,it,vi} from 'vitest'
import {cleanup,render,screen} from '@testing-library/react'
import {MemoryRouter} from 'react-router-dom'
import {QueryClient,QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {BrowserTracking,trackingPage} from './index'
vi.mock('@/api/client',()=>({api:vi.fn()}))
const mockAPI=vi.mocked(api)
beforeEach(()=>mockAPI.mockReset());afterEach(cleanup)
it('never includes object IDs, search queries, credential or personal-setting screens',()=>{
  expect(trackingPage('/messages/private-message')).toBe('/app/messages/:id')
  expect(trackingPage('/accounts/secret-account')).toBe('/app/accounts/:id')
  for(const path of ['/login','/setup','/error','/keys','/settings','/settings/signatures','/accounts/private/preferences'])expect(trackingPage(path)).toBe('')
})
it('runs opt-in tracking only in an opaque frame with no parent access or referrer',async()=>{
  mockAPI.mockResolvedValue({active:true,frame_url:'/api/tracking/frame?page=%2Fapp%2Fmail'})
  render(<QueryClientProvider client={new QueryClient()}><MemoryRouter initialEntries={['/mail?q=secret']}><BrowserTracking/></MemoryRouter></QueryClientProvider>)
  const frame=await screen.findByTitle('개인정보 격리 방문 통계')
  expect(frame).toHaveAttribute('sandbox','allow-scripts');expect(frame).toHaveAttribute('referrerpolicy','no-referrer')
  expect(mockAPI).toHaveBeenCalledWith('/api/tracking?page=%2Fapp%2Fmail',expect.anything())
  expect(document.querySelector('script')).toBeNull()
})
it('does not load tracking configuration on a credential screen',()=>{
  render(<QueryClientProvider client={new QueryClient()}><MemoryRouter initialEntries={['/login']}><BrowserTracking/></MemoryRouter></QueryClientProvider>)
  expect(mockAPI).not.toHaveBeenCalled();expect(document.querySelector('iframe')).toBeNull()
})

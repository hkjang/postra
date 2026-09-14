import {render, screen} from '@testing-library/react'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {MemoryRouter} from 'react-router-dom'
import {vi, it, expect} from 'vitest'
import {api} from '@/api/client'
import {SentPage} from './index'
vi.mock('@/api/client', () => ({api: vi.fn()}))
it('shows nested outbound status and safe links including uncertain delivery', async () => {
  vi.mocked(api).mockResolvedValue([{subject: '전달 확인이 필요한 메일', to: ['team@corp.local'], outbound: {id: 'out_1', draft_id: 'drf_1', status: 'send_uncertain', attempts: 1, created_at: 1780000000}}])
  render(<QueryClientProvider client={new QueryClient({defaultOptions: {queries: {retry: false}}})}><MemoryRouter><SentPage/></MemoryRouter></QueryClientProvider>)
  expect(await screen.findByRole('link', {name: '전달 확인이 필요한 메일'})).toHaveAttribute('href', '/drafts/drf_1')
  expect(screen.getByText('결과 확인 필요')).toBeInTheDocument()
  expect(screen.getByText(/중복 발송을 피하려면/)).toBeInTheDocument()
  expect(screen.getByText('team@corp.local')).toBeInTheDocument()
})

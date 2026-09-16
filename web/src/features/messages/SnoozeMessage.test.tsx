import {afterEach, beforeEach, expect, it, vi} from 'vitest'
import {act, cleanup, fireEvent, render, screen, waitFor} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'
import {api} from '@/api/client'
import {toast} from 'sonner'
import {SnoozeMessage} from './SnoozeMessage'
import {snoozeTimestamp} from './snooze'
import {createWorkspaceShortcuts} from '@/lib/workspace-shortcuts'

vi.mock('@/api/client', () => ({api: vi.fn()}))
vi.mock('sonner', () => ({toast: {success: vi.fn()}}))
const now = new Date(2026, 8, 16, 12, 0)
const success = {succeeded: 1, failed: 0, results: [{message_id: 'm1', ok: true}]}
beforeEach(() => {vi.clearAllMocks(); vi.useFakeTimers({toFake: ['Date']}); vi.setSystemTime(now)})
afterEach(() => {cleanup(); vi.useRealTimers()})
function mount(until = 0) {
  const cache = new QueryClient({defaultOptions: {queries: {retry: false}, mutations: {retry: false}}})
  const invalidate = vi.spyOn(cache, 'invalidateQueries')
  render(<QueryClientProvider client={cache}><SnoozeMessage messageID="m1" snoozedUntil={until}/></QueryClientProvider>)
  return {cache, invalidate}
}

it('opens an accessible dialog without writes and explicitly schedules only the selected mail', async () => {
  const user = userEvent.setup(); vi.mocked(api).mockResolvedValue(success)
  const {invalidate} = mount()
  await user.click(screen.getByRole('button', {name: '나중에 다시 보기'}))
  expect(screen.getByRole('dialog', {name: '메일을 나중에 다시 보기'})).toBeInTheDocument()
  expect(api).not.toHaveBeenCalled()
  await user.selectOptions(screen.getByLabelText('다시 볼 시각'), 'hour')
  await user.click(screen.getByRole('button', {name: '다시 보기 예약'}))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  expect(api).toHaveBeenCalledWith('/api/messages/batch', {method: 'POST', body: {message_ids: ['m1'], action: 'snooze', snoozed_until: snoozeTimestamp('hour', '', now)}})
  expect(api).toHaveBeenCalledTimes(1)
  for (const queryKey of [['message', 'm1'], ['messages'], ['work'], ['thread']]) expect(invalidate).toHaveBeenCalledWith({queryKey})
  expect(toast.success).toHaveBeenCalledTimes(1)
})

it('keeps an existing reminder visible and clears it only through an explicit cancel operation', async () => {
  const user = userEvent.setup(); vi.mocked(api).mockResolvedValue(success)
  mount(snoozeTimestamp('1', '', now))
  expect(screen.getByRole('status')).toHaveTextContent('다시 보기 예약:')
  await user.click(screen.getByRole('button', {name: '다시 보기 변경'}))
  expect(screen.getByText(/현재 예약:/)).toBeInTheDocument()
  await user.click(screen.getByRole('button', {name: '예약 해제'}))
  await waitFor(() => expect(api).toHaveBeenCalledWith('/api/messages/batch', {method: 'POST', body: {message_ids: ['m1'], action: 'unsnooze'}}))
  await waitFor(() => expect(toast.success).toHaveBeenCalledWith('다시 보기 예약을 해제했습니다.'))
})

it('validates custom local dates before sending and permits correction', async () => {
  const user = userEvent.setup(); vi.mocked(api).mockResolvedValue(success); mount()
  await user.click(screen.getByRole('button', {name: '나중에 다시 보기'}))
  await user.selectOptions(screen.getByLabelText('다시 볼 시각'), 'custom')
  fireEvent.change(screen.getByLabelText('직접 지정 날짜·시각'), {target: {value: '2026-09-15T10:00'}})
  await user.click(screen.getByRole('button', {name: '다시 보기 예약'}))
  expect(screen.getByRole('alert')).toHaveTextContent('현재보다 이후')
  expect(api).not.toHaveBeenCalled()
  fireEvent.change(screen.getByLabelText('직접 지정 날짜·시각'), {target: {value: '2026-09-18T14:30'}})
  await user.click(screen.getByRole('button', {name: '다시 보기 예약'}))
  await waitFor(() => expect(api).toHaveBeenCalledWith('/api/messages/batch', {method: 'POST', body: {message_ids: ['m1'], action: 'snooze', snoozed_until: new Date(2026, 8, 18, 14, 30).getTime() / 1000}}))
})

it.each([
  {succeeded: 0, failed: 1, results: [{message_id: 'm1', ok: false, error: '권한을 확인해 주세요.'}]},
  {succeeded: 1, failed: 0, results: [{message_id: 'another', ok: true}]},
  {succeeded: 0, failed: 0, results: []},
])('keeps the dialog and old reminder when results do not confirm success: %j', async payload => {
  const user = userEvent.setup(); vi.mocked(api).mockResolvedValue(payload)
  const {invalidate} = mount(snoozeTimestamp('1', '', now))
  await user.click(screen.getByRole('button', {name: '다시 보기 변경'}))
  await user.click(screen.getByRole('button', {name: '예약 해제'}))
  await screen.findByRole('alert')
  expect(screen.getByRole('dialog')).toBeInTheDocument()
  expect(screen.getByText(/현재 예약:/)).toBeInTheDocument()
  expect(invalidate).not.toHaveBeenCalled()
  expect(toast.success).not.toHaveBeenCalled()
})

it('prevents duplicate requests and closing while a save is pending', async () => {
  let resolve!: (value: unknown) => void
  vi.mocked(api).mockImplementation(() => new Promise(done => {resolve = done}))
  const user = userEvent.setup(); mount()
  await user.click(screen.getByRole('button', {name: '나중에 다시 보기'}))
  const form = screen.getByRole('button', {name: '다시 보기 예약'}).closest('form')!
  act(() => {fireEvent.submit(form); fireEvent.submit(form)})
  await waitFor(() => expect(api).toHaveBeenCalledTimes(1))
  expect(screen.getByRole('button', {name: '다시 보기 닫기'})).toBeDisabled()
  await user.keyboard('{Escape}')
  expect(screen.getByRole('dialog')).toBeInTheDocument()
  await act(async () => resolve(success))
  await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
})

it('closing without saving writes nothing and reopening starts with a fresh form', async () => {
  const user = userEvent.setup(); mount()
  await user.click(screen.getByRole('button', {name: '나중에 다시 보기'}))
  await user.selectOptions(screen.getByLabelText('다시 볼 시각'), 'custom')
  fireEvent.change(screen.getByLabelText('직접 지정 날짜·시각'), {target: {value: '2026-09-18T14:30'}})
  await user.click(screen.getByRole('button', {name: '다시 보기 닫기'}))
  expect(api).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', {name: '나중에 다시 보기'}))
  expect(screen.getByLabelText('다시 볼 시각')).toHaveValue('1')
  expect(screen.queryByLabelText('직접 지정 날짜·시각')).not.toBeInTheDocument()
})

it('lets the dialog own keyboard shortcuts instead of changing the underlying mail', async () => {
  const user = userEvent.setup(); mount()
  await user.click(screen.getByRole('button', {name: '나중에 다시 보기'}))
  const actions = {navigate: vi.fn(), openCommand: vi.fn(), showHelp: vi.fn(), closePanels: vi.fn(() => false), mail: vi.fn(() => true)}
  const shortcuts = createWorkspaceShortcuts(actions)
  for (const key of ['r', 'a', 'f', 'e', 'j', 'k', 'Escape', 'c']) shortcuts.keydown(new KeyboardEvent('keydown', {key}))
  shortcuts.keydown(new KeyboardEvent('keydown', {key: 'k', ctrlKey: true}))
  expect(actions.mail).not.toHaveBeenCalled(); expect(actions.navigate).not.toHaveBeenCalled()
  expect(actions.openCommand).not.toHaveBeenCalled(); expect(actions.closePanels).not.toHaveBeenCalled()
  expect(api).not.toHaveBeenCalled()
})

import {act, cleanup, render, screen} from '@testing-library/react'
import {useQuery} from '@tanstack/react-query'
import {afterEach, it, expect} from 'vitest'
import {UserDataProvider} from './providers'

afterEach(cleanup)

it('never reuses mailbox query cache after the identity key changes', async () => {
  let account = 'A'
  function Mailbox() {const query = useQuery({queryKey: ['messages'], queryFn: async () => account}); return <p>{query.data || 'loading'}</p>}
  const view = render(<UserDataProvider key="A"><Mailbox/></UserDataProvider>)
  expect(await screen.findByText('A')).toBeInTheDocument()
  account = 'B'
  view.rerender(<UserDataProvider key="B"><Mailbox/></UserDataProvider>)
  expect(screen.queryByText('A')).not.toBeInTheDocument()
  expect(await screen.findByText('B')).toBeInTheDocument()
})

it('late responses from a previous identity cannot populate the new user cache', async () => {
  let finishOld: (value: string) => void = () => {}
  const old = new Promise<string>(resolve => {finishOld = resolve})
  function Mailbox({user}: {user: string}) {const query = useQuery({queryKey: ['messages'], queryFn: () => user === 'A' ? old : Promise.resolve('B')}); return <p>{query.data || 'loading'}</p>}
  const view = render(<UserDataProvider key="A"><Mailbox user="A"/></UserDataProvider>)
  view.rerender(<UserDataProvider key="B"><Mailbox user="B"/></UserDataProvider>)
  expect(await screen.findByText('B')).toBeInTheDocument()
  await act(async () => {finishOld('A')})
  expect(screen.queryByText('A')).not.toBeInTheDocument()
  expect(screen.getByText('B')).toBeInTheDocument()
})

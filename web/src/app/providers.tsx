import {useState, type ReactNode} from 'react'
import {QueryClient, QueryClientProvider} from '@tanstack/react-query'

export function newQueryClient() {
  return new QueryClient({defaultOptions: {queries: {staleTime: 15000,
    retry: (count, error) => !('status' in error && [401, 403, 404].includes(Number(error.status))) && count < 1,
    refetchOnWindowFocus: false}, mutations: {retry: false}}})
}

// This provider is keyed by the authenticated user ID. Neither cached mailbox
// data nor in-flight requests are reused when another identity signs in.
export function UserDataProvider({children}: {children: ReactNode}) {
  const [client] = useState(newQueryClient)
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>
}

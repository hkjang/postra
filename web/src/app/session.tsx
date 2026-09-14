import {createContext, useContext} from 'react'
export interface Principal {user_id: string; login_id: string; display_name: string; role: string; auth_method: string}
export const SessionContext = createContext<Principal | undefined>(undefined)
export function useSession() { return useContext(SessionContext) }

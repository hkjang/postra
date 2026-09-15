import {useQuery} from '@tanstack/react-query'
import {api} from '@/api/client'
import type {EffectiveSetting, SettingsView as WireSettingsView} from '@/api/contracts.generated'

export type SettingField = EffectiveSetting
export type SettingsView = WireSettingsView
export function usePersonalPreferences(accountID?: string) {
  const path = accountID ? `/api/accounts/${encodeURIComponent(accountID)}/preferences` : '/api/preferences'
  const query = useQuery({queryKey: ['preferences', accountID || 'user'], queryFn: ({signal}) => api<SettingsView>(path, {signal}), staleTime: 30000, refetchOnWindowFocus: true})
  return {...query, value: (key: string, fallback = '') => query.data?.fields?.find(field => field.key === key)?.value ?? fallback,
    locked: (key: string) => query.data?.fields?.find(field => field.key === key)?.locked ?? false}
}

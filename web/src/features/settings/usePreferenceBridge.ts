import {useEffect} from 'react'
import {useQueryClient} from '@tanstack/react-query'
import {toast} from 'sonner'
import {api} from '@/api/client'
import {usePreferences} from '@/stores/preferences'
import {usePersonalPreferences, type SettingsView} from './preferences'

// Server identity-scoped preferences are authoritative. Local storage is only
// a non-sensitive paint hint while the authenticated workspace is loading.
export function usePreferenceBridge() {
  const prefs = usePersonalPreferences()
  const cache = useQueryClient()
  const {setTheme, setDensity} = usePreferences()
  const theme = prefs.value('ui.theme', 'system')
  const density = prefs.value('ui.density', 'comfortable')
  const textSize = prefs.value('ui.text_size', 'standard') === 'large' ? 'large' : 'standard'
  const language = prefs.value('ui.language', 'ko')
  const dateFormat = prefs.value('ui.date_format', 'relative')
  useEffect(() => {
    const media = window.matchMedia?.('(prefers-color-scheme: dark)')
    const apply = () => {
      const resolved = theme === 'dark' || theme === 'system' && media?.matches ? 'dark' : 'light'
      document.documentElement.dataset.theme = resolved
      document.documentElement.dataset.density = density
      document.documentElement.dataset.textSize = textSize
      document.documentElement.dataset.dateFormat = dateFormat
      document.documentElement.lang = language
      setTheme(resolved); setDensity(density === 'compact' ? 'compact' : 'comfortable')
    }
    apply()
    media?.addEventListener?.('change', apply)
    return () => media?.removeEventListener?.('change', apply)
  }, [theme, density, textSize, language, dateFormat, setTheme, setDensity])
  async function update(key: string, value: string) {
    if (prefs.locked(key)) {toast.error('관리자가 강제한 설정입니다.'); return}
    try {
      const saved = await api<SettingsView>('/api/preferences', {method: 'PATCH', body: {values: {[key]: value}}})
      cache.setQueryData(['preferences', 'user'], saved)
    } catch (error) {toast.error(error instanceof Error ? error.message : '개인 설정을 저장하지 못했습니다.')}
  }
  return {...prefs, update}
}

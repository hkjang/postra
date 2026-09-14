import {create} from 'zustand'
import {persist} from 'zustand/middleware'
type Preferences = {
  theme: 'light'|'dark'; density: 'comfortable'|'compact';
  setTheme: (theme: 'light'|'dark') => void; setDensity: (density: 'comfortable'|'compact') => void;
}
export const usePreferences = create<Preferences>()(persist(set => ({
  theme: window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light', density: 'comfortable',
  setTheme: theme => set({theme}), setDensity: density => set({density}),
}), {name: 'postra-ui-preferences'}))

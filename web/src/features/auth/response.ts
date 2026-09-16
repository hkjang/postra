import {z} from 'zod'
import {parseResponse} from '@/api/response'

const principal = z.object({
  user_id: z.string(), login_id: z.string(), display_name: z.string(), role: z.string(), auth_method: z.string(),
})
const session = z.object({
  authenticated: z.boolean(), auth_enabled: z.boolean(), login_url: z.string(), principal: principal.optional(),
  local_auth: z.boolean().optional(), token_required: z.boolean().optional(),
  oidc_url: z.string().optional(), oidc_auto_login: z.boolean().optional(),
  setup_url: z.string().optional(), sso_error: z.string().optional(),
}).refine(value => !value.authenticated || value.principal !== undefined)

export function parseBrowserSession(value: unknown) {return parseResponse(session, value)}

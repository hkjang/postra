import {useCallback, useEffect, useRef, useState} from 'react'
import {z} from 'zod'
import {api} from '@/api/client'
import {parseResponse} from '@/api/response'
import {Badge} from '@/components/ui'

const modelLimitsSchema = z.object({
  model: z.string(),
  context_length: z.number().int().nonnegative(),
  max_output_tokens: z.number().int().nonnegative(),
  source: z.enum(['models', 'config']),
  status: z.enum(['detected', 'unavailable', 'manual', 'model_not_found']),
})
const diagnosticSchema = z.object({
  ok: z.boolean(),
  message: z.string(),
  model: z.string().optional(),
  // The existing combined embedding/vector diagnostic has separate timings.
  latency_ms: z.number().nonnegative().optional(),
  limits: modelLimitsSchema.optional(),
})
export type ConnectionResult = z.infer<typeof diagnosticSchema>
export const parseConnectionResult = (value: unknown) => parseResponse(diagnosticSchema, value)

const limitLabels = {detected: '자동 감지', unavailable: '설정값 대체', manual: '수동 설정', model_not_found: '모델 미발견 · 설정값 대체'}
const limitHelp = {
  detected: '서버가 제공한 컨텍스트 한도입니다.',
  unavailable: '모델 메타데이터를 확인하지 못해 설정한 한도를 사용합니다. 연결 실패를 뜻하지는 않습니다.',
  manual: '자동 감지가 꺼져 있어 설정한 한도를 사용합니다.',
  model_not_found: '서버 모델 목록에 선택한 모델이 없어 설정한 한도를 사용합니다.',
}
const tokens = (value: number) => value > 0 ? `${value.toLocaleString('ko-KR')} 토큰` : '미제공'

export function ConnectionDiagnostic({result, metadataOnly = false, showModelLimits = true}: {result: ConnectionResult; metadataOnly?: boolean; showModelLimits?: boolean}) {
  return <section className="connection-diagnostic stack" role="status">
    <p><Badge variant={metadataOnly || result.ok ? 'secondary' : 'destructive'}>{metadataOnly ? '모델 한도 조회' : result.ok ? '연결 성공' : '연결 확인 필요'}</Badge> {result.message}{result.latency_ms !== undefined && ` · ${result.latency_ms}ms`}</p>
    {showModelLimits && (result.limits ? <div className="model-limits stack">
      <div className="row"><Badge variant="outline">{result.limits.context_length > 0 ? limitLabels[result.limits.status] : result.limits.status === 'manual' ? '수동 설정 · 컨텍스트 미제공' : result.limits.status === 'model_not_found' ? '모델 미발견 · 컨텍스트 미제공' : '컨텍스트 미제공'}</Badge><span>{result.limits.model || result.model || '모델 미지정'}</span></div>
      <div>컨텍스트: {tokens(result.limits.context_length)} · 최대 출력: {tokens(result.limits.max_output_tokens)}</div>
      <small className="muted">{result.limits.context_length > 0 ? limitHelp[result.limits.status] : '모델 컨텍스트 한도가 제공되지 않았습니다.'} 컨텍스트 출처: {result.limits.context_length <= 0 ? '미제공' : result.limits.source === 'models' ? '서버 /models' : '설정값'}.</small>
      {result.limits.max_output_tokens > 0 && <small className="muted">최대 출력은 설정·작업별 한도와 서버 한도 중 작은 값을 적용합니다.</small>}
    </div> : result.model ? <small className="muted">모델: {result.model} · 모델 한도 정보 미제공</small> : null)}
  </section>
}

// Invalidate synchronously when an input changes, and ignore late responses
// even when the server (or a transport mock) cannot honor cancellation.
export function useConnectionProbe(scope: string) {
  const [result, setResult] = useState<{data: ConnectionResult; metadataOnly: boolean}>()
  const [error, setError] = useState<unknown>()
  const [busy, setBusy] = useState(false)
  const request = useRef(0)
  const controller = useRef<AbortController | undefined>(undefined)
  const clear = useCallback(() => {
    request.current++
    controller.current?.abort()
    setResult(undefined); setError(undefined); setBusy(false)
  }, [])
  useEffect(() => {
    clear()
    return () => {request.current++; controller.current?.abort()}
  }, [scope, clear])
  const run = async (path: string, body?: unknown, metadataOnly = false) => {
    clear()
    const id = request.current
    const pending = new AbortController()
    controller.current = pending
    setBusy(true)
    try {
      const data = parseConnectionResult(await api<unknown>(path, {method: 'POST', ...(body !== undefined ? {body} : {}), signal: pending.signal}))
      if (id === request.current) setResult({data, metadataOnly})
    } catch (err) {
      if (id === request.current) setError(err)
    } finally {
      if (id === request.current) setBusy(false)
    }
  }
  return {result, error, busy, clear, run}
}

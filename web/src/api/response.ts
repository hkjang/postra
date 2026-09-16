import {z} from 'zod'

// Never include invalid server payloads in errors: they can contain mail or
// credentials. A malformed success response is an error, not an empty inbox.
export class InvalidResponseError extends Error {
  readonly code = 'invalid_response'
  constructor() {
    super('서버 응답 형식을 확인할 수 없습니다. 다시 시도해 주세요.')
    this.name = 'InvalidResponseError'
  }
}

export function parseResponse<T>(schema: z.ZodType<T>, value: unknown): T {
  const result = schema.safeParse(value)
  if (!result.success) throw new InvalidResponseError()
  return result.data
}

// Go's nil slices encode as null in older servers. Only that known empty-list
// representation is normalized; missing fields and malformed items still fail.
export function nullableList<T>(item: z.ZodType<T>) {
  return z.array(item).nullable().transform(items => items ?? [])
}

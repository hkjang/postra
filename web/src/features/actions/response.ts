import {z} from 'zod'
import {nullableList, parseResponse} from '@/api/response'

const optionalText = z.string().nullish().transform(value => value ?? undefined)
const actionCard = z.object({
  id: z.string(), message_id: z.string(), type: z.string(), title: z.string(), status: z.string(),
  detail: optionalText, due: optionalText, assignee: optionalText,
  confidence: z.number().optional(),
})
const actionCards = z.object({cards: nullableList(actionCard)})
export function parseActionCards(value: unknown) {return parseResponse(actionCards, value)}

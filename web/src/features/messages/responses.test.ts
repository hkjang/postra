import {expect, it} from 'vitest'
import {InvalidResponseError} from '@/api/response'
import {actionCardsResponse, batchResponse, calendarResponse, createdDraftID, messagePageResponse, messageViewResponse, repliesResponse} from './responses'
import {mailDate} from './types'

it('normalizes known legacy null collections without accepting malformed success DTOs',()=>{
  expect(repliesResponse({suggestions:null})).toEqual({suggestions:[]})
  expect(actionCardsResponse({cards:null})).toEqual({cards:[]})
  expect(calendarResponse({events:null})).toEqual({events:[]})
  expect(messagePageResponse({messages:null}).messages).toEqual([])
  for(const [parse, payload] of [
    [repliesResponse,{}],[repliesResponse,{suggestions:[null]}],[repliesResponse,{suggestions:{password:'never-echo'}}],
    [actionCardsResponse,{cards:[null]}],[calendarResponse,{events:[{title:{secret:'never-echo'},start:'today'}]}],
    [messagePageResponse,{messages:[null]}],[messagePageResponse,{}],[messageViewResponse,{message:null}],
    [batchResponse,{failed:0,succeeded:1,results:null}],[createdDraftID,{draft:null}],
  ] as const) expect(()=>parse(payload)).toThrow(InvalidResponseError)
  try {repliesResponse({suggestions:{password:'never-echo'}})} catch(error) {expect(String(error)).not.toContain('never-echo')}
})
it('invalid or extreme timestamps never crash message rendering',()=>{
  for(const date of [0,NaN,Infinity,Number.MAX_VALUE]) expect(mailDate(date)).toBe('날짜 없음')
})

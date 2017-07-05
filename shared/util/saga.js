// @flow
import mapValues from 'lodash/mapValues'
import isEqual from 'lodash/isEqual'
import map from 'lodash/map'
import forEach from 'lodash/forEach'
import {buffers, channel} from 'redux-saga'
import {
  actionChannel,
  take,
  call,
  put,
  race,
  fork,
  takeEvery,
  takeLatest,
  cancelled,
} from 'redux-saga/effects'
import {globalError} from '../constants/config'
import {convertToError} from '../util/errors'

import type {Action} from '../constants/types/flux'
import type {ChannelConfig, ChannelMap, SagaGenerator, Channel} from '../constants/types/saga'

type SagaMap = {[key: string]: any}
type Effect = any

function createChannelMap<T>(channelConfig: ChannelConfig<T>): ChannelMap<T> {
  return mapValues(channelConfig, v => {
    return channel(v())
  })
}

function putOnChannelMap<T>(channelMap: ChannelMap<T>, k: string, v: T): void {
  const c = channelMap[k]
  if (c) {
    c.put(v)
  } else {
    console.error('Trying to put, but no registered channel for', k)
  }
}

// TODO type this properly
function effectOnChannelMap<T>(effectFn: any, channelMap: ChannelMap<T>, k: string): any {
  const c = channelMap[k]
  if (c) {
    return effectFn(c)
  } else {
    console.error('Trying to do effect, but no registered channel for', k)
  }
}

function takeFromChannelMap<T>(channelMap: ChannelMap<T>, k: string): any {
  return effectOnChannelMap(take, channelMap, k)
}

// Map a chanmap method -> channel to a saga map method -> saga using the given effectFn
function mapSagasToChanMap<T>(
  effectFn: (c: Channel<T>, saga: SagaGenerator<any, any>) => any,
  sagaMap: SagaMap,
  channelMap: ChannelMap<T>
): Array<Effect> {
  // Check that all method names are accounted for
  if (!isEqual(Object.keys(channelMap).sort(), Object.keys(sagaMap).sort())) {
    console.warn('Missing or extraneous saga handlers')
  }
  return map(sagaMap, (saga, methodName) =>
    effectOnChannelMap(c => effectFn(c, saga), channelMap, methodName)
  )
}

function closeChannelMap<T>(channelMap: ChannelMap<T>): void {
  forEach(channelMap, c => c.close())
}

function singleFixedChannelConfig<T>(ks: Array<string>): ChannelConfig<T> {
  return ks.reduce((acc, k) => {
    acc[k] = () => buffers.expanding(1)
    return acc
  }, {})
}

function safeTakeEvery(pattern: string | Array<any> | Function, worker: Function, ...args: Array<any>) {
  const wrappedWorker = function*(...args) {
    try {
      yield call(worker, ...args)
    } catch (error) {
      // Convert to global error so we don't kill the takeEvery loop
      yield put(dispatch => {
        dispatch({
          payload: convertToError(error),
          type: globalError,
        })
      })
    } finally {
      if (yield cancelled()) {
        console.log('safeTakeEvery cancelled')
      }
    }
  }

  return takeEvery(pattern, wrappedWorker, ...args)
}

function safeTakeLatestWithCatch(
  pattern: string | Array<any> | Function | Channel<any>,
  catchHandler: Function,
  worker: Function | SagaGenerator<any, any>,
  ...args: Array<any>
) {
  const wrappedWorker = function*(...args) {
    try {
      yield call(worker, ...args)
    } catch (error) {
      // Convert to global error so we don't kill the takeLatest loop
      yield put({
        payload: convertToError(error),
        type: globalError,
      })
      yield call(catchHandler, error)
    } finally {
      if (yield cancelled()) {
        console.log('safeTakeLatestWithCatch cancelled')
      }
    }
  }

  return takeLatest(pattern, wrappedWorker, ...args)
}

function safeTakeLatest(
  pattern: string | Array<any> | Function | Channel<any>,
  worker: Function | SagaGenerator<any, any>,
  ...args: Array<any>
) {
  return safeTakeLatestWithCatch(pattern, () => {}, worker, ...args)
}

function cancelWhen(predicate: (originalAction: Action, checkAction: Action) => boolean, worker: Function) {
  const wrappedWorker = function*(action: Action): SagaGenerator<any, any> {
    yield race({
      result: call(worker, action),
      cancel: take((checkAction: Action) => predicate(action, checkAction)),
    })
  }

  return wrappedWorker
}

function safeTakeSerially(pattern: string | Array<any> | Function, worker: Function, ...args: Array<any>) {
  const wrappedWorker = function*(...args) {
    try {
      yield call(worker, ...args)
    } catch (error) {
      // Convert to global error so we don't kill the loop
      yield put(dispatch => {
        dispatch({
          payload: convertToError(error),
          type: globalError,
        })
      })
    } finally {
      if (yield cancelled()) {
        console.log('safeTakeSerially cancelled')
      }
    }
  }

  return fork(function*() {
    const chan = yield actionChannel(pattern, buffers.expanding(10))
    while (true) {
      const action = yield take(chan)
      yield call(wrappedWorker, action, ...args)
    }
  })
}

export {
  cancelWhen,
  closeChannelMap,
  createChannelMap,
  effectOnChannelMap,
  mapSagasToChanMap,
  putOnChannelMap,
  safeTakeEvery,
  safeTakeLatest,
  safeTakeLatestWithCatch,
  safeTakeSerially,
  singleFixedChannelConfig,
  takeFromChannelMap,
}

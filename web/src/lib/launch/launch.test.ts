import { describe, expect, it, vi, beforeEach } from 'vitest'
import {
  describeLaunchFailures,
  failedLaunchChecks,
  primarySetupTarget,
  setupTargetForCheck,
} from './preflight'
import { pickTradingExchange, pickTradingModel } from './resolve'
import type { LaunchCheck, LaunchPreflightResult } from './types'
import type { AIModel, Exchange } from '../../types'

vi.mock('../api', () => ({ api: {} }))
vi.mock('../api/helpers', () => ({
  API_BASE: '',
  httpClient: { get: vi.fn(), post: vi.fn() },
}))

function preflightResult(checks: LaunchCheck[]): LaunchPreflightResult {
  return {
    ready: checks.every((check) => check.status !== 'failed'),
    checks,
    min_trading_usdc: 12,
    checked_at: new Date().toISOString(),
  }
}

describe('setupTargetForCheck', () => {
  it('routes AI model problems to the model setup anchor', () => {
    expect(setupTargetForCheck({ id: 'ai_model', status: 'failed' })).toBe(
      'model'
    )
  })

  it('routes exchange config/account problems to hyperliquid setup', () => {
    expect(
      setupTargetForCheck({ id: 'exchange_config', status: 'failed' })
    ).toBe('hyperliquid')
    expect(
      setupTargetForCheck({ id: 'exchange_account', status: 'failed' })
    ).toBe('hyperliquid')
  })

  it('routes funding shortfalls to the funds anchor', () => {
    expect(
      setupTargetForCheck({ id: 'exchange_funds', status: 'failed' })
    ).toBe('hyperliquid-funds')
  })

  it('has no anchor for strategy problems', () => {
    expect(setupTargetForCheck({ id: 'strategy', status: 'failed' })).toBeNull()
  })
})

describe('primarySetupTarget', () => {
  it('returns the anchor for the first failing check', () => {
    const result = preflightResult([
      { id: 'ai_model', status: 'ok' },
      { id: 'exchange_funds', status: 'failed', message: 'Low margin.' },
    ])
    expect(primarySetupTarget(result)).toBe('hyperliquid-funds')
  })

  it('returns the model anchor for a missing model', () => {
    const result = preflightResult([
      { id: 'ai_model', status: 'failed', message: 'No model configured.' },
      { id: 'exchange_funds', status: 'warning' },
    ])
    expect(primarySetupTarget(result)).toBe('model')
  })

  it('returns null when everything passes', () => {
    const result = preflightResult([
      { id: 'ai_model', status: 'ok' },
      { id: 'exchange_funds', status: 'warning' },
    ])
    expect(primarySetupTarget(result)).toBeNull()
  })
})

describe('describeLaunchFailures / failedLaunchChecks', () => {
  it('collects only failed checks and joins their messages', () => {
    const result = preflightResult([
      { id: 'ai_model', status: 'failed', message: 'No model configured.' },
      { id: 'exchange_funds', status: 'warning', message: 'Testnet funds.' },
      { id: 'exchange_account', status: 'failed', message: 'Bad key.' },
      { id: 'strategy', status: 'skipped' },
    ])
    expect(failedLaunchChecks(result)).toHaveLength(2)
    expect(describeLaunchFailures(result)).toBe(
      'No model configured. Bad key.'
    )
  })
})

describe('pickTradingModel', () => {
  const base: Partial<AIModel> = { enabled: true }

  it('returns any enabled model with a credential', () => {
    const models = [
      { ...base, id: 'openai', provider: 'openai', has_api_key: true },
      { ...base, id: 'deepseek', provider: 'deepseek', has_api_key: false },
    ] as AIModel[]
    expect(pickTradingModel(models)?.id).toBe('openai')
  })

  it('ignores models without a credential', () => {
    const models = [
      { ...base, id: 'deepseek', provider: 'deepseek' },
    ] as AIModel[]
    expect(pickTradingModel(models)).toBeNull()
  })

  it('returns null when nothing usable exists', () => {
    const models = [
      { id: 'x', provider: 'openai', enabled: false, has_api_key: true },
      { id: 'y', provider: 'deepseek', enabled: true },
    ] as AIModel[]
    expect(pickTradingModel(models)).toBeNull()
  })
})

describe('pickTradingExchange', () => {
  beforeEach(() => vi.clearAllMocks())

  it('requires enabled + key + builder approval + wallet address', () => {
    const ready = {
      id: 'hl',
      exchange_type: 'hyperliquid',
      enabled: true,
      has_api_key: true,
      hyperliquidBuilderApproved: true,
      hyperliquidWalletAddr: '0x1',
    } as unknown as Exchange
    expect(pickTradingExchange([ready])?.id).toBe('hl')

    const unapproved = {
      ...ready,
      hyperliquidBuilderApproved: false,
    } as unknown as Exchange
    expect(pickTradingExchange([unapproved])).toBeNull()

    const noAddr = {
      ...ready,
      hyperliquidWalletAddr: ' ',
    } as unknown as Exchange
    expect(pickTradingExchange([noAddr])).toBeNull()
  })
})

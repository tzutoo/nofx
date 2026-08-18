import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  AlertCircle,
  ArrowRight,
  CheckCircle2,
  ExternalLink,
  Loader2,
  RefreshCw,
  ShieldCheck,
  Wallet,
  Zap,
} from 'lucide-react'
import { toast } from 'sonner'
import { buildDashboardPath } from '../../router/paths'
import {
  ensureClaw402Strategy,
  launchAutopilot,
} from '../../lib/launch/launchAutopilot'
import { runLaunchPreflight } from '../../lib/launch/preflight'
import { pickTradingModel } from '../../lib/launch/resolve'
import type { LaunchPreflightResult } from '../../lib/launch/types'
import type {
  AIModel,
  Exchange,
  ExchangeAccountState,
  TraderInfo,
} from '../../types'
import { HyperliquidWalletConnect } from '../common/HyperliquidWalletConnect'

type LaunchStepStatus = 'ready' | 'action' | 'blocked'

interface AutopilotLaunchPanelProps {
  models: AIModel[]
  exchanges: Exchange[]
  exchangeAccountStates: Record<string, ExchangeAccountState>
  traders?: TraderInfo[]
  isLoggedIn: boolean
  language: string
  onRefresh: () => Promise<void>
  onOpenHyperliquidConfig?: () => void
}

const MIN_TRADING_USDC = 12

function parseNumber(value?: string | number) {
  if (typeof value === 'number') return Number.isFinite(value) ? value : 0
  if (!value) return 0
  const parsed = Number(value.replace(/[,$\s]/g, ''))
  return Number.isFinite(parsed) ? parsed : 0
}

function shortAddress(address?: string) {
  if (!address) return '--'
  return `${address.slice(0, 6)}…${address.slice(-4)}`
}

function formatUSDC(value: number) {
  return new Intl.NumberFormat('en-US', {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value)
}

export function AutopilotLaunchPanel({
  models,
  exchanges,
  exchangeAccountStates,
  traders = [],
  isLoggedIn,
  language,
  onRefresh,
  onOpenHyperliquidConfig,
}: AutopilotLaunchPanelProps) {
  const navigate = useNavigate()
  const [launching, setLaunching] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const isZh = language === 'zh'

  const tradingModel = useMemo(
    () => pickTradingModel(models),
    [models]
  )

  const hyperliquidExchange = useMemo(
    () =>
      exchanges.find(
        (exchange) =>
          exchange.exchange_type === 'hyperliquid' &&
          exchange.enabled &&
          Boolean(exchange.hyperliquidWalletAddr) &&
          Boolean(exchange.hyperliquidBuilderApproved)
      ) || null,
    [exchanges]
  )

  // Any hyperliquid account (even partially configured) is enough for the
  // server preflight — it reports exactly which prerequisite is missing.
  const preflightExchange = useMemo(
    () =>
      hyperliquidExchange ||
      exchanges.find((exchange) => exchange.exchange_type === 'hyperliquid') ||
      null,
    [exchanges, hyperliquidExchange]
  )

  // Server-side preflight is the source of truth for balances: it queries the
  // exchange live (30s server cache) instead of trusting a client snapshot.
  // Poll while the panel is visible so deposits show up without a refresh.
  const [preflight, setPreflight] = useState<LaunchPreflightResult | null>(null)
  const modelId = tradingModel?.id
  const preflightExchangeId = preflightExchange?.id
  useEffect(() => {
    if (!isLoggedIn || !modelId || !preflightExchangeId) {
      setPreflight(null)
      return
    }
    let cancelled = false
    const check = async () => {
      try {
        const result = await runLaunchPreflight({
          ai_model_id: modelId,
          exchange_id: preflightExchangeId,
        })
        if (!cancelled) setPreflight(result)
      } catch {
        // keep the last known result; client-derived fallbacks still render
      }
    }
    void check()
    const timer = setInterval(() => void check(), 20000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [isLoggedIn, modelId, preflightExchangeId])

  const preflightCheck = (id: string) =>
    preflight?.checks.find((check) => check.id === id)

  const hyperliquidConnected = Boolean(hyperliquidExchange)
  const exchangeState = hyperliquidExchange
    ? exchangeAccountStates[hyperliquidExchange.id]
    : undefined
  const accountCheck = preflightCheck('exchange_account')
  const tradingFundsCheck = preflightCheck('exchange_funds')
  const tradingBalance =
    tradingFundsCheck?.actual ??
    parseNumber(exchangeState?.available_balance ?? exchangeState?.total_equity)
  const minTradingUSDC = preflight?.min_trading_usdc ?? MIN_TRADING_USDC
  const tradingBalanceReady =
    hyperliquidConnected &&
    (accountCheck && tradingFundsCheck
      ? accountCheck.status === 'ok' && tradingFundsCheck.status !== 'failed'
      : exchangeState?.status === 'ok' && tradingBalance >= minTradingUSDC)

  const autopilotTrader = useMemo(
    () =>
      traders.find((trader) => trader.trader_name === 'NOFX Autopilot') ||
      traders.find((trader) =>
        (trader.strategy_name || '').toLowerCase().includes('claw402')
      ) ||
      null,
    [traders]
  )

  const allReady = hyperliquidConnected && tradingBalanceReady

  const refreshEverything = async () => {
    setRefreshing(true)
    try {
      await onRefresh()
    } finally {
      setRefreshing(false)
    }
  }

  const handleLaunch = async () => {
    if (!tradingModel || !hyperliquidExchange) return
    setLaunching(true)
    try {
      // Shared launch path (same as Strategy Studio): server preflight with
      // fresh balances first, then strategy provisioning, then create/start.
      const outcome = await launchAutopilot({
        ensureStrategy: ensureClaw402Strategy,
        scanIntervalMinutes: 5,
      })

      if (!outcome.ok) {
        toast.error(outcome.message)
        if (outcome.kind === 'preflight') {
          setPreflight(outcome.preflight)
        }
        if (outcome.kind !== 'error') {
          if (outcome.setupTarget === 'hyperliquid') {
            onOpenHyperliquidConfig?.()
          }
        }
        await refreshEverything()
        return
      }

      if (outcome.warning) {
        toast.warning(outcome.warning)
      }
      await onRefresh()
      toast.success('NOFX Autopilot is running')
      navigate(buildDashboardPath(outcome.traderId))
    } finally {
      setLaunching(false)
    }
  }

  const steps: Array<{
    title: string
    detail: string
    status: LaunchStepStatus
    meta?: string
    action?: JSX.Element
  }> = [
    {
      title: 'Step 1 · Connect Hyperliquid',
      detail:
        'Approve NOFX once with your crypto wallet (Rabby or MetaMask). This lets the AI place trades for you — it can never withdraw your money.',
      status: hyperliquidConnected ? 'ready' : 'action',
      meta: hyperliquidExchange?.hyperliquidWalletAddr
        ? `${shortAddress(hyperliquidExchange.hyperliquidWalletAddr)} · authorized`
        : 'A few clicks + 3 wallet signatures',
      action: (
        <button
          type="button"
          onClick={() => onOpenHyperliquidConfig?.()}
          className="inline-flex items-center gap-1.5 text-xs font-semibold text-nofx-gold hover:text-nofx-accent"
        >
          <Wallet className="h-3.5 w-3.5" />
          Open
        </button>
      ),
    },
    {
      title: 'Step 2 · Add trading money ($12+)',
      detail:
        'Deposit USDC into your Hyperliquid account (app.hyperliquid.xyz → Deposit, USDC on Arbitrum). This is what the AI trades with — start small, you can add more anytime.',
      status: tradingBalanceReady
        ? 'ready'
        : hyperliquidConnected
          ? 'action'
          : 'blocked',
      meta: hyperliquidConnected
        ? `${formatUSDC(tradingBalance)} USDC available${
            tradingBalanceReady ? '' : ` · needs ≥ ${minTradingUSDC} USDC`
          }`
        : 'Finish step 1 first',
    },
    {
      title: 'Step 3 · Press start',
      detail:
        'The AI reads the market every few minutes, picks its trades, and manages them on its own. Watch every decision live on the dashboard — stop it with one click anytime.',
      status: allReady ? 'ready' : 'blocked',
      meta: autopilotTrader?.is_running
        ? 'Running — open the dashboard to watch'
        : autopilotTrader
          ? 'Ready to start'
          : allReady
            ? 'Everything is ready — press the button'
            : 'Unlocks when steps 1–2 are green',
    },
  ]

  const renderPrimaryAction = () => {
    if (!hyperliquidConnected) {
      return (
        <button
          type="button"
          onClick={() => {
            if (onOpenHyperliquidConfig) {
              onOpenHyperliquidConfig()
            } else {
              document
                .getElementById('hyperliquid-quick-connect')
                ?.scrollIntoView({ behavior: 'smooth', block: 'start' })
            }
          }}
          className="inline-flex items-center justify-center gap-2 rounded-lg bg-nofx-gold px-4 py-3 text-sm font-bold text-white hover:bg-nofx-accent"
        >
          Connect Hyperliquid
          <ArrowRight className="h-4 w-4" />
        </button>
      )
    }

    if (!tradingBalanceReady) {
      return (
        <a
          href="https://app.hyperliquid.xyz/"
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center justify-center gap-2 rounded-lg bg-nofx-gold px-4 py-3 text-sm font-bold text-white hover:bg-nofx-accent"
        >
          Deposit USDC on Hyperliquid
          <ExternalLink className="h-4 w-4" />
        </a>
      )
    }

    if (autopilotTrader?.is_running) {
      return (
        <button
          type="button"
          onClick={() =>
            navigate(buildDashboardPath(autopilotTrader.trader_id))
          }
          className="inline-flex items-center justify-center gap-2 rounded-lg bg-nofx-success px-4 py-3 text-sm font-bold text-white hover:bg-nofx-success/80"
        >
          Open dashboard
          <ArrowRight className="h-4 w-4" />
        </button>
      )
    }

    return (
      <button
        type="button"
        onClick={() => void handleLaunch()}
        disabled={launching || !allReady}
        className="inline-flex items-center justify-center gap-2 rounded-lg bg-nofx-gold px-4 py-3 text-sm font-bold text-white hover:bg-nofx-accent disabled:cursor-not-allowed disabled:opacity-60"
      >
        {launching ? (
          <Loader2 className="h-4 w-4 animate-spin" />
        ) : (
          <Zap className="h-4 w-4" />
        )}
        Start NOFX Autopilot
      </button>
    )
  }

  return (
    <section
      id="autopilot-launch-panel"
      className="overflow-hidden rounded-xl border border-nofx-gold/20 bg-nofx-bg-lighter"
    >
      <div className="grid gap-0 xl:grid-cols-[1.05fr_0.95fr]">
        <div className="p-5 md:p-6">
          <div className="mb-5 flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
            <div>
              <div className="mb-2 inline-flex items-center gap-2 rounded-full border border-nofx-gold/25 bg-nofx-gold/10 px-3 py-1 text-[11px] font-semibold uppercase tracking-[0.18em] text-nofx-gold">
                <ShieldCheck className="h-3.5 w-3.5" />
                Guided Launch
              </div>
              <h2 className="text-2xl font-bold tracking-tight text-nofx-text md:text-3xl">
                Start NOFX Autopilot in minutes
              </h2>
              <p className="mt-2 max-w-2xl text-sm leading-6 text-nofx-text-muted">
                Three small steps, about $12 total. Configure an AI model, add
                trading money, and the AI trades for you — you can stop it
                anytime.
              </p>
            </div>
            <div className="flex flex-wrap gap-2">
              <button
                type="button"
                onClick={() => void refreshEverything()}
                disabled={refreshing}
                className="inline-flex items-center justify-center gap-2 rounded-lg border border-nofx-gold/20 bg-nofx-bg-deeper px-3 py-2 text-xs font-semibold text-nofx-text-muted hover:text-nofx-text disabled:opacity-60"
              >
                <RefreshCw
                  className={`h-3.5 w-3.5 ${refreshing ? 'animate-spin' : ''}`}
                />
                Refresh
              </button>
              {renderPrimaryAction()}
            </div>
          </div>

          <div className="grid gap-3 md:grid-cols-2">
            {steps.map((step, index) => (
              <div
                key={step.title}
                className="rounded-lg border border-nofx-gold/20 bg-nofx-bg p-4"
              >
                <div className="flex items-start gap-3">
                  <div
                    className={`mt-0.5 flex h-8 w-8 shrink-0 items-center justify-center rounded-lg border text-sm font-bold ${
                      step.status === 'ready'
                        ? 'border-nofx-success/30 bg-nofx-success/15 text-nofx-success'
                        : step.status === 'action'
                          ? 'border-nofx-gold/30 bg-nofx-gold/15 text-nofx-gold'
                          : 'border-nofx-gold/20 bg-nofx-bg-deeper text-nofx-text-muted'
                    }`}
                  >
                    {step.status === 'ready' ? (
                      <CheckCircle2 className="h-4 w-4" />
                    ) : step.status === 'action' ? (
                      index + 1
                    ) : (
                      <AlertCircle className="h-4 w-4" />
                    )}
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center justify-between gap-2">
                      <h3 className="font-semibold text-nofx-text">
                        {step.title}
                      </h3>
                      {step.action}
                    </div>
                    <p className="mt-1 text-xs leading-5 text-nofx-text-muted">
                      {step.detail}
                    </p>
                    <div className="mt-3 font-mono text-xs text-nofx-gold/90">
                      {step.meta}
                    </div>
                  </div>
                </div>
              </div>
            ))}
          </div>
        </div>

        <aside className="border-t border-nofx-gold/20 bg-nofx-bg p-5 md:p-6 xl:border-l xl:border-t-0">
          <div className="mb-4 flex items-center gap-2 text-sm font-semibold text-nofx-text">
            <Wallet className="h-4 w-4 text-nofx-gold" />
            Hyperliquid setup
          </div>
          {hyperliquidConnected ? (
            <div className="rounded-lg border border-nofx-success/25 bg-nofx-success/10 p-4">
              <div className="flex items-center gap-2 text-sm font-semibold text-nofx-success">
                <CheckCircle2 className="h-4 w-4" />
                Trading authorization is ready
              </div>
              <div className="mt-2 font-mono text-xs text-nofx-success/90">
                {shortAddress(hyperliquidExchange?.hyperliquidWalletAddr)}
              </div>
              <p className="mt-3 text-xs leading-5 text-nofx-text-muted">
                Funds stay in your Hyperliquid account. NOFX only stores the
                authorized Agent key required for automated execution.
              </p>
            </div>
          ) : (
            <div>
              <div id="hyperliquid-quick-connect">
                <HyperliquidWalletConnect
                  language={isZh ? 'zh' : 'en'}
                  isLoggedIn={isLoggedIn}
                  variant="inline"
                  onSaved={refreshEverything}
                />
              </div>
            </div>
          )}
        </aside>
      </div>
    </section>
  )
}

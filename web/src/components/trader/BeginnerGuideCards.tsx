import { Brain, Landmark, Rocket, Sparkles } from 'lucide-react'

interface BeginnerGuideCardsProps {
  language: string
  modelReady: boolean
  exchangeReady: boolean
  strategyReady: boolean
  traderReady: boolean
  canCreateTrader: boolean
  onOpenModelConfig: () => void
  onOpenExchange: () => void
  onOpenStrategy: () => void
  onCreateTrader: () => void
}

export function BeginnerGuideCards({
  language,
  modelReady,
  exchangeReady,
  strategyReady,
  traderReady,
  canCreateTrader,
  onOpenModelConfig,
  onOpenExchange,
  onOpenStrategy,
  onCreateTrader,
}: BeginnerGuideCardsProps) {
  const isZh = language === 'zh'

  const cards = [
    {
      key: 'model',
      icon: Brain,
      title: isZh ? '1. Configure an AI model' : '1. Configure an AI model',
      desc: isZh
        ? 'Pick a model and add its API key. The AI drives every trade for you.'
        : 'Pick a model and add its API key. The AI drives every trade for you.',
      meta: isZh
        ? 'Paid model with an API key'
        : 'Paid model with an API key',
      ready: modelReady,
      actionLabel: modelReady
        ? isZh
          ? 'Configured'
          : 'Configured'
        : isZh
          ? 'Configure'
          : 'Configure',
      onAction: onOpenModelConfig,
      disabled: false,
    },
    {
      key: 'exchange',
      icon: Landmark,
      title: isZh ? '2. Add Exchange' : '2. Add Exchange',
      desc: isZh
        ? 'Connect an exchange so the AI can actually place trades.'
        : 'Connect an exchange so the AI can actually place trades.',
      meta: exchangeReady
        ? isZh
          ? 'Ready'
          : 'Ready'
        : isZh
          ? 'Binance / OKX / Bybit / Hyperliquid'
          : 'Binance / OKX / Bybit / Hyperliquid',
      ready: exchangeReady,
      actionLabel: exchangeReady
        ? isZh
          ? 'Manage'
          : 'Manage'
        : isZh
          ? 'Configure'
          : 'Configure',
      onAction: onOpenExchange,
      disabled: false,
    },
    {
      key: 'strategy',
      icon: Sparkles,
      title: isZh ? '3. Pick Strategy' : '3. Pick Strategy',
      desc: isZh
        ? 'You can start with a default strategy and fine-tune later.'
        : 'You can start with a default strategy and fine-tune later.',
      meta: strategyReady
        ? isZh
          ? 'Strategy ready'
          : 'Strategy ready'
        : isZh
          ? 'Optional, but worth a quick look'
          : 'Optional, but worth a quick look',
      ready: strategyReady,
      actionLabel: isZh ? 'Open strategy' : 'Open strategy',
      onAction: onOpenStrategy,
      disabled: false,
    },
    {
      key: 'trader',
      icon: Rocket,
      title: isZh ? '4. Create Trader' : '4. Create Trader',
      desc: isZh
        ? 'Last step: bind your model and exchange, then start running.'
        : 'Last step: bind your model and exchange, then start running.',
      meta: traderReady
        ? isZh
          ? 'Trader created, you can add more'
          : 'Trader created, you can add more'
        : canCreateTrader
          ? isZh
            ? 'Ready to create'
            : 'Ready to create'
        : isZh
          ? 'Finish the first three steps first'
          : 'Finish the first three steps first',
      ready: traderReady,
      actionLabel: traderReady
        ? isZh
          ? 'Create another'
          : 'Create another'
        : isZh
          ? 'Create now'
          : 'Create now',
      onAction: onCreateTrader,
      disabled: !canCreateTrader,
    },
  ]

  return (
    <section className="space-y-4 rounded-[28px] border border-nofx-gold/20 bg-nofx-bg-lighter p-5">
      <div className="flex items-center justify-between gap-4">
        <div>
          <div className="text-xs font-semibold uppercase tracking-[0.3em] text-nofx-gold/80">
            {isZh ? 'Quickstart' : 'Quickstart'}
          </div>
          <h2 className="mt-1 text-xl font-bold text-nofx-text">
            {isZh
              ? 'Follow these 4 steps to get started fast'
              : 'Follow these 4 steps to get started fast'}
          </h2>
        </div>
        {/* <div className="rounded-full border border-nofx-gold/20 bg-nofx-bg-deeper px-3 py-1 text-xs text-nofx-text-muted">
          {isZh ? 'Hidden in advanced mode' : 'Hidden in advanced mode'}
        </div> */}
      </div>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
        {cards.map((card) => {
          const Icon = card.icon
          return (
            <div
              key={card.key}
              className="rounded-[22px] border border-nofx-gold/20 bg-nofx-bg-deeper p-4"
            >
              <div className="flex items-center justify-between gap-3">
                <div className="flex h-11 w-11 items-center justify-center rounded-2xl bg-nofx-gold/10 text-nofx-gold">
                  <Icon className="h-5 w-5" />
                </div>
                <span
                  className={`rounded-full px-2.5 py-1 text-[10px] font-bold uppercase tracking-[0.22em] ${
                    card.ready
                      ? 'bg-nofx-success/15 text-nofx-success'
                      : 'bg-nofx-bg-deeper text-nofx-text-muted'
                  }`}
                >
                  {card.ready
                    ? isZh
                      ? 'Ready'
                      : 'Ready'
                    : isZh
                      ? 'Pending'
                      : 'Pending'}
                </span>
              </div>

              <h3 className="mt-4 text-base font-semibold text-nofx-text">
                {card.title}
              </h3>
              <p className="mt-2 min-h-[72px] text-sm leading-6 text-nofx-text-muted">
                {card.desc}
              </p>
              <div className="mt-3 text-xs text-nofx-text-muted">{card.meta}</div>

              <button
                type="button"
                onClick={card.onAction}
                disabled={card.disabled}
                className={`mt-5 w-full rounded-2xl px-4 py-3 text-sm font-semibold transition ${
                  card.disabled
                    ? 'cursor-not-allowed bg-nofx-bg-deeper text-nofx-text-muted'
                    : 'bg-nofx-gold text-white hover:bg-nofx-gold/90'
                }`}
              >
                {card.actionLabel}
              </button>
            </div>
          )
        })}
      </div>
    </section>
  )
}

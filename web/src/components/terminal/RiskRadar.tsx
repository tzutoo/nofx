import { useMemo, useState } from 'react'
import type { Position } from '../../types'

/**
 * RiskRadar renders derived risk telemetry for the live trading book — long /
 * short exposure split, leverage usage vs config cap, margin utilization,
 * single-name concentration, max drawdown and position count — as a dense stack
 * of self-explanatory gauge rows. Each row reads as: bilingual label · value +
 * unit · a thin gauge bar · and a one-glance verdict tag. Cream-themed
 * Bloomberg/terminal cockpit.
 *
 * Every value is DERIVED from real props. No synthetic or random data; missing
 * inputs collapse to 0 and divides are guarded.
 */

const C_AMBER = '#c8860b' // escalation tint between green and red

function fmtUsd(n: number): string {
  const a = Math.abs(n)
  const sign = n < 0 ? '-' : ''
  if (a >= 1e6) return `${sign}$${(a / 1e6).toFixed(2)}M`
  if (a >= 1e3) return `${sign}$${(a / 1e3).toFixed(1)}K`
  if (a >= 100) return `${sign}$${a.toFixed(0)}`
  // small PnLs matter here: -$0.22 must not render as -$0
  return `${sign}$${a.toFixed(2)}`
}

function pct(n: number): string {
  return `${n.toFixed(1)}%`
}

function isLong(side: string): boolean {
  return (side || '').toLowerCase() === 'long'
}

// margin utilization escalates green → amber → red as the book fills up
function utilColor(p: number): string {
  if (p > 80) return 'var(--tm-dn)'
  if (p >= 50) return C_AMBER
  return 'var(--tm-up)'
}

interface RiskRadarProps {
  positions?: Position[]
  account?: { total_equity?: number; unrealized_profit?: number; margin_used_pct?: number } | null
  config?: { btc_eth_leverage?: number; altcoin_leverage?: number; max_positions?: number } | null
  /** max_drawdown_pct is a percent (18.5 = -18.5%), not a fraction. */
  fullStats?: { max_drawdown_pct?: number; profit_factor?: number; sharpe_ratio?: number; win_rate?: number } | null
}

export function RiskRadar({ positions, account, config, fullStats }: RiskRadarProps) {
  const pos = positions ?? []

  const m = useMemo(() => {
    const equity = account?.total_equity ?? 0

    let longNotional = 0
    let shortNotional = 0
    let levSum = 0
    let levCount = 0
    let maxLev = 0
    let marginSum = 0
    let topNotional = 0

    for (const p of pos) {
      const px = p.mark_price || p.entry_price || 0
      const notional = Math.abs(p.quantity || 0) * px
      if (isLong(p.side)) longNotional += notional
      else shortNotional += notional

      const lev = p.leverage || 0
      if (lev > 0) {
        levSum += lev
        levCount += 1
        if (lev > maxLev) maxLev = lev
      }
      marginSum += p.margin_used || 0
      if (notional > topNotional) topNotional = notional
    }

    const totalNotional = longNotional + shortNotional
    const netNotional = longNotional - shortNotional
    const longShare = totalNotional > 0 ? (longNotional / totalNotional) * 100 : 0
    const shortShare = totalNotional > 0 ? (shortNotional / totalNotional) * 100 : 0

    const avgLev = levCount > 0 ? levSum / levCount : 0
    const configMax = Math.max(config?.btc_eth_leverage ?? 0, config?.altcoin_leverage ?? 0)
    const levUse = configMax > 0 ? Math.min(100, (avgLev / configMax) * 100) : 0

    const marginPct =
      account?.margin_used_pct != null
        ? account.margin_used_pct
        : equity > 0
          ? (marginSum / equity) * 100
          : 0

    const concentration = totalNotional > 0 ? (topNotional / totalNotional) * 100 : 0

    const drawdown = fullStats?.max_drawdown_pct ?? 0

    const count = pos.length
    const maxPositions = config?.max_positions ?? 0
    const countUse = maxPositions > 0 ? Math.min(100, (count / maxPositions) * 100) : 0

    const upnl = account?.unrealized_profit ?? 0

    return {
      longNotional,
      shortNotional,
      netNotional,
      longShare,
      shortShare,
      totalNotional,
      avgLev,
      maxLev,
      configMax,
      levUse,
      marginPct,
      concentration,
      drawdown,
      count,
      maxPositions,
      countUse,
      upnl,
    }
  }, [pos, account, config, fullStats])

  const hasData = pos.length > 0 || account != null
  if (!hasData) {
    return <div className="tm-sc" style={{ padding: '16px 0' }}>No live risk data.</div>
  }

  // ── one-glance verdicts ──────────────────────────────────────────────
  // Net exposure bias: Long-lean / Short-lean / Balanced by the long-share spread around 50%.
  const biasSkew = m.longShare - m.shortShare
  const exposureTag: Verdict =
    m.totalNotional === 0
      ? { text: 'Flat', tone: 'muted', title: 'No positions — net exposure is flat.' }
      : biasSkew > 15
        ? { text: 'Long-lean', tone: 'up', title: 'Long share exceeds short by >15pts.' }
        : biasSkew < -15
          ? { text: 'Short-lean', tone: 'dn', title: 'Short share exceeds long by >15pts.' }
          : { text: 'Balanced', tone: 'ink', title: 'Long/short shares within 15pts.' }

  // Leverage: info-only — leverage is pinned to the cap by design, so it carries
  // no risk color (the Margin Used row owns risk color). Flag 'At cap' when the
  // average is at the cap so the user sees there is no leverage headroom.
  const atCap = m.configMax > 0 && m.avgLev > 0 && m.avgLev >= m.configMax - 0.5
  const levTag: Verdict =
    m.configMax === 0 || m.avgLev === 0
      ? { text: '—', tone: 'muted', title: 'Info only; no positions.' }
      : atCap
        ? { text: 'At cap', tone: 'amber', title: "Info only — avg leverage equals the cap (no headroom). Risk color lives in Margin Used." }
        : { text: 'Below cap', tone: 'muted', title: 'Info only — avg leverage below the cap.' }

  // Margin used: Ample / Tight / Risky.
  const marginTag: Verdict =
    m.marginPct > 80
      ? { text: 'Risky', tone: 'dn', title: 'Margin >80% of equity.' }
      : m.marginPct >= 50
        ? { text: 'Tight', tone: 'amber', title: 'Margin 50–80% of equity.' }
        : { text: 'Ample', tone: 'up', title: 'Margin <50% of equity.' }

  // Concentration: Spread / Concentrated. With MaxPositions=2 the minimum
  // concentration is 50% (two balanced positions), so 'Concentrated' is reserved
  // for a dominant single position (>70%) or exactly one position.
  const concTag: Verdict =
    m.totalNotional === 0
      ? { text: '—', tone: 'muted', title: 'No positions held.' }
      : m.concentration > 70 || m.count === 1
        ? { text: 'Concentrated', tone: 'amber', title: '>70% of total notional, or only 1 position held.' }
        : { text: 'Spread', tone: 'up', title: '≤70% of total notional across ≥2 positions.' }

  // Drawdown: historical max drawdown (positive %). Calm <5%, Caution 5-20%,
  // Deep >20%. Framed as a historical-magnitude stat, not a live alarm.
  const ddTag: Verdict =
    m.drawdown <= 5
      ? { text: 'Calm', tone: 'up', title: 'Historical max drawdown <5%.' }
      : m.drawdown >= 20
        ? { text: 'Deep', tone: 'dn', title: 'Historical max drawdown ≥20%.' }
        : { text: 'Caution', tone: 'amber', title: 'Historical max drawdown 5–20%.' }

  // Positions: Room / Full.
  const countTag: Verdict =
    m.maxPositions === 0
      ? { text: `${m.count}`, tone: 'muted', title: `${m.count} positions; no cap configured.` }
      : m.count >= m.maxPositions
        ? { text: 'Full', tone: 'amber', title: 'Positions at/above the cap.' }
        : { text: 'Room', tone: 'up', title: 'Positions below the cap.' }

  return (
    <div style={{ fontFamily: 'var(--tm-mono)' }}>
      {/* header */}
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 8, marginBottom: 1 }}>
        <span className="tm-px" style={{ fontSize: 11 }}>Risk radar</span>
        <span
          className="tm-sc"
          style={{ marginLeft: 'auto', color: m.totalNotional > 0 ? 'var(--tm-up)' : 'var(--tm-muted)' }}
        >
          {m.totalNotional > 0 ? '● live' : '○ flat'}
        </span>
      </div>
      <div className="tm-sc" style={{ fontSize: 9, marginBottom: 8 }}>
        Risk radar · live position-risk check
      </div>

      {/* Net exposure — diverging long/short split, the visual centerpiece */}
      <div style={{ marginBottom: 9, paddingBottom: 9, borderBottom: '1px solid var(--tm-hair)' }}>
        <div style={{ display: 'flex', alignItems: 'baseline', marginBottom: 4 }}>
          <Label zh="Net exposure" en="NET EXPOSURE" />
          <Tag verdict={exposureTag} />
          <span className="tm-mono" style={{ marginLeft: 'auto', fontSize: 11, color: 'var(--tm-ink)' }}>
            long {pct(m.longShare)}
            <span style={{ color: 'var(--tm-muted)' }}> / </span>
            short {pct(m.shortShare)}
          </span>
        </div>
        <div style={{ display: 'flex', height: 7, background: 'var(--tm-hair)', overflow: 'hidden' }}>
          <div style={{ width: `${m.longShare}%`, background: 'var(--tm-up)' }} />
          <div style={{ width: `${m.shortShare}%`, background: 'var(--tm-dn)' }} />
        </div>
        <div className="tm-mono" style={{ display: 'flex', justifyContent: 'space-between', fontSize: 9, marginTop: 3 }}>
          <span style={{ color: 'var(--tm-up)' }}>long {fmtUsd(m.longNotional)}</span>
          <span style={{ color: 'var(--tm-ink-2)' }}>
            net <b style={{ color: m.netNotional >= 0 ? 'var(--tm-up)' : 'var(--tm-dn)' }}>{fmtUsd(m.netNotional)}</b>
          </span>
          <span style={{ color: 'var(--tm-dn)' }}>short {fmtUsd(m.shortNotional)}</span>
        </div>
      </div>

      {/* gauge rows */}
      <GaugeRow
        zh="Leverage"
        en="LEVERAGE"
        value={`${m.avgLev.toFixed(1)}× avg`}
        sub={`/ ${m.maxLev > 0 ? `${m.maxLev.toFixed(0)}×` : '—'} peak · ${m.configMax > 0 ? `${m.configMax}×` : '—'} cap`}
        fill={m.levUse}
        color="var(--tm-muted)" // info-only; risk color lives in the Margin Used row
        verdict={levTag}
      />
      <GaugeRow
        zh="Margin used"
        en="MARGIN USED"
        value={pct(m.marginPct)}
        sub="of equity"
        fill={Math.min(100, Math.max(0, m.marginPct))}
        color={utilColor(m.marginPct)}
        verdict={marginTag}
      />
      <GaugeRow
        zh="Concentration"
        en="CONCENTRATION"
        value={pct(m.concentration)}
        sub="top-position share"
        fill={m.concentration}
        color={concTag.tone === 'amber' ? C_AMBER : 'var(--tm-up)'}
        verdict={concTag}
      />
      <GaugeRow
        zh="Drawdown"
        en="MAX DRAWDOWN"
        value={`-${pct(m.drawdown)}`}
        sub="peak drawdown"
        fill={Math.min(100, m.drawdown)}
        color="var(--tm-red)"
        verdict={ddTag}
        valueColor="var(--tm-dn)"
      />
      <GaugeRow
        zh="Positions"
        en="POSITIONS"
        value={m.maxPositions > 0 ? `${m.count} / ${m.maxPositions}` : `${m.count}`}
        sub="held / cap"
        fill={m.maxPositions > 0 ? m.countUse : 0}
        color={countTag.tone === 'amber' ? C_AMBER : 'var(--tm-up)'}
        verdict={countTag}
      />

      {/* unrealized PnL footer */}
      <div
        style={{
          display: 'flex',
          alignItems: 'baseline',
          marginTop: 8,
          paddingTop: 7,
          borderTop: '1px solid var(--tm-hair)',
        }}
      >
        <Label zh="Unrealized PnL" en="UNREALIZED PNL" />
        <span
          className="tm-mono"
          style={{ marginLeft: 'auto', fontSize: 13, fontWeight: 700, color: m.upnl >= 0 ? 'var(--tm-up)' : 'var(--tm-dn)' }}
        >
          {m.upnl >= 0 ? '+' : ''}{fmtUsd(m.upnl)}
        </span>
      </div>
    </div>
  )
}

// ── verdict tag ────────────────────────────────────────────────────────
type Tone = 'up' | 'dn' | 'amber' | 'ink' | 'muted'

interface Verdict {
  text: string
  tone: Tone
  title?: string
}

function toneColor(tone: Tone): string {
  switch (tone) {
    case 'up':
      return 'var(--tm-up)'
    case 'dn':
      return 'var(--tm-dn)'
    case 'amber':
      return C_AMBER
    case 'ink':
      return 'var(--tm-ink)'
    default:
      return 'var(--tm-muted)'
  }
}

function Tag({ verdict }: { verdict: Verdict }) {
  const c = toneColor(verdict.tone)
  const [open, setOpen] = useState(false)
  return (
    <span
      style={{
        position: 'relative',
        display: 'inline-block',
        marginLeft: 6,
        padding: '0 4px',
        fontSize: 9,
        lineHeight: '13px',
        letterSpacing: '0.08em',
        color: c,
        border: `1px solid ${c}`,
        borderRadius: 2,
        cursor: verdict.title ? 'help' : 'default',
      }}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
    >
      {verdict.text}
      {open && verdict.title && (
        <span
          style={{
            position: 'absolute',
            bottom: '100%',
            left: '50%',
            transform: 'translateX(-50%)',
            marginBottom: 4,
            padding: '4px 7px',
            fontSize: 10,
            lineHeight: '14px',
            whiteSpace: 'nowrap',
            color: '#f0f0f0',
            background: '#2a2a2a',
            border: '1px solid #5a5a5a',
            borderRadius: 3,
            zIndex: 30,
            pointerEvents: 'none',
            boxShadow: '0 2px 8px rgba(0,0,0,0.6)',
          }}
        >
          {verdict.title}
        </span>
      )}
    </span>
  )
}

// ── bilingual label block ──────────────────────────────────────────────
function Label({ zh, en }: { zh: string; en: string }) {
  return (
    <span style={{ display: 'inline-flex', flexDirection: 'column', lineHeight: 1.2 }}>
      <span style={{ fontSize: 11, color: 'var(--tm-ink)', fontWeight: 600 }}>{zh}</span>
      <span className="tm-sc" style={{ fontSize: 8, letterSpacing: '0.12em' }}>{en}</span>
    </span>
  )
}

interface GaugeRowProps {
  zh: string
  en: string
  value: string
  sub?: string
  fill: number
  color: string
  verdict: Verdict
  valueColor?: string
}

function GaugeRow({ zh, en, value, sub, fill, color, verdict, valueColor }: GaugeRowProps) {
  const w = Math.min(100, Math.max(0, fill))
  return (
    <div style={{ marginBottom: 9 }}>
      <div style={{ display: 'flex', alignItems: 'center', marginBottom: 4 }}>
        <Label zh={zh} en={en} />
        <Tag verdict={verdict} />
        <span
          className="tm-mono"
          style={{ marginLeft: 'auto', fontSize: 12, fontWeight: 600, color: valueColor ?? 'var(--tm-ink)' }}
        >
          {value}
        </span>
      </div>
      <div style={{ height: 5, background: 'var(--tm-hair)', overflow: 'hidden' }}>
        <div style={{ width: `${w}%`, height: '100%', background: color, transition: 'width 0.2s ease-out' }} />
      </div>
      {sub && (
        <div className="tm-sc" style={{ fontSize: 8, marginTop: 2 }}>{sub}</div>
      )}
    </div>
  )
}

export default RiskRadar

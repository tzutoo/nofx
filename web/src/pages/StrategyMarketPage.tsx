import { useLanguage } from '../contexts/LanguageContext'
import { t } from '../i18n/translations'
import { BlockedEmbed } from '../components/common/BlockedEmbed'

export function StrategyMarketPage() {
  const { language } = useLanguage()

  return (
    <BlockedEmbed
      title={t('strategyMarket', language) || 'Strategy Market'}
      url="https://vergex.trade/explore"
    />
  )
}

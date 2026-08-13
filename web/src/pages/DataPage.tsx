import { useLanguage } from '../contexts/LanguageContext'
import { t } from '../i18n/translations'
import { BlockedEmbed } from '../components/common/BlockedEmbed'

export function DataPage() {
  const { language } = useLanguage()

  return (
    <BlockedEmbed
      title={t('dataCenter', language)}
      url="https://vergex.trade/trending"
    />
  )
}

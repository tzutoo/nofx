import { ExternalLink, Globe } from 'lucide-react'
import { useLanguage } from '../../contexts/LanguageContext'
import { t } from '../../i18n/translations'

export function BlockedEmbed({ title, url }: { title: string; url: string }) {
  const { language } = useLanguage()

  return (
    <div className="h-[calc(100vh-64px)] w-full flex items-center justify-center p-6">
      <div
        className="w-full max-w-md p-8 rounded-2xl text-center"
        style={{
          background: '#F7F4EC',
          border: '1px solid rgba(26,24,19,0.14)',
        }}
      >
        <div
          className="w-14 h-14 mx-auto mb-4 rounded-xl flex items-center justify-center"
          style={{ background: 'rgba(224, 72, 59, 0.1)', color: '#E0483B' }}
        >
          <Globe className="w-7 h-7" />
        </div>
        <h2 className="text-lg font-bold mb-2" style={{ color: '#1A1813' }}>
          {title}
        </h2>
        <p className="text-sm mb-6" style={{ color: '#8A8478' }}>
          {t('embedBlockedNote', language)}
        </p>
        <a
          href={url}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-2 px-5 py-3 rounded-xl text-sm font-bold transition-all hover:scale-105"
          style={{ background: '#E0483B', color: '#fff' }}
        >
          {t('openInNewTab', language)}
          <ExternalLink className="w-4 h-4" />
        </a>
      </div>
    </div>
  )
}

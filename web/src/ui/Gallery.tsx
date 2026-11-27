import { type ReactNode, useEffect, useState } from 'react'
import { useT } from '../i18n'
import type { GalleryModule } from './GalleryParts'
import { BRANDS, COLOR_ROLES, DEFAULT_BRAND } from './roles'
import { SegmentedControl } from './SegmentedControl'
import './themes/harbor.css'
import './Gallery.css'

const modules = import.meta.glob<GalleryModule>('./*.gallery.tsx', { eager: true })
const SECTIONS = Object.values(modules).sort((a, b) => a.title.localeCompare(b.title))

type Theme = 'system' | 'light' | 'dark'
type Brand = (typeof BRANDS)[number]
type Dir = 'ltr' | 'rtl'

const TYPE_ROLES = ['display', 'title', 'section', 'ui', 'small', 'caption', 'code'] as const

function useHtml(theme: Theme, brand: Brand, dir: Dir) {
  useEffect(() => {
    const html = document.documentElement
    if (theme === 'system') delete html.dataset.theme
    else html.dataset.theme = theme
    if (brand === DEFAULT_BRAND) delete html.dataset.brand
    else html.dataset.brand = brand
    html.dir = dir
  }, [theme, brand, dir])
}

function initial<T extends string>(key: string, allowed: readonly T[], fallback: T): T {
  const v = new URLSearchParams(window.location.search).get(key)
  return allowed.includes(v as T) ? (v as T) : fallback
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="ui-gallery-section" aria-label={title}>
      <h2 className="ui-gallery-title" style={{ font: 'var(--type-section)', letterSpacing: 'normal' }}>
        {title}
      </h2>
      {children}
    </section>
  )
}

export default function Gallery() {
  const t = useT()
  const [theme, setTheme] = useState<Theme>(() => initial('theme', ['system', 'light', 'dark'], 'system'))
  const [brand, setBrand] = useState<Brand>(() => initial('brand', BRANDS, DEFAULT_BRAND))
  const [dir, setDir] = useState<Dir>(() => initial('dir', ['ltr', 'rtl'], 'ltr'))
  useHtml(theme, brand, dir)

  return (
    <main className="ui-gallery">
      <header className="ui-gallery-head">
        <div>
          <h1 className="ui-gallery-heading" style={{ font: 'var(--type-display)' }}>
            {t('gallery.title')}
          </h1>
          <p className="ui-gallery-lede">{t('gallery.lede')}</p>
          <p className="ui-gallery-lede">{t('gallery.hint')}</p>
        </div>
        <div className="ui-gallery-switches">
          <SegmentedControl
            size="sm"
            label={t('gallery.theme')}
            value={theme}
            onChange={setTheme}
            options={[
              { value: 'system', label: t('gallery.system') },
              { value: 'light', label: t('gallery.light'), icon: 'sun' },
              { value: 'dark', label: t('gallery.dark'), icon: 'moon' },
            ]}
          />
          <SegmentedControl
            size="sm"
            label={t('gallery.brand')}
            value={brand}
            onChange={setBrand}
            options={[
              { value: 'mint', label: t('gallery.mint') },
              { value: 'harbor', label: t('gallery.harbor') },
            ]}
          />
          <SegmentedControl
            size="sm"
            label={t('gallery.direction')}
            value={dir}
            onChange={setDir}
            options={[
              { value: 'ltr', label: t('gallery.ltr') },
              { value: 'rtl', label: t('gallery.rtl') },
            ]}
          />
        </div>
      </header>

      <Section title={t('gallery.colours')}>
        <div className="ui-gallery-swatches">
          {COLOR_ROLES.map((role) => (
            <div key={role} className="ui-gallery-swatch">
              <span className="ui-gallery-chip" style={{ background: `var(--color-${role})` }} />
              <code>{role}</code>
            </div>
          ))}
        </div>
      </Section>

      <Section title={t('gallery.type')}>
        <div>
          {TYPE_ROLES.map((role) => (
            <div key={role} className="ui-gallery-type">
              <code>{`--type-${role}`}</code>
              <span style={{ font: `var(--type-${role})` }}>
                {role === 'display' || role === 'title'
                  ? t('gallery.typePage')
                  : role === 'section'
                    ? t('gallery.typeSection')
                    : role === 'small'
                      ? t('gallery.typeSmall')
                      : role === 'caption'
                        ? t('gallery.typeLabel')
                        : role === 'code'
                          ? t('gallery.typeCode')
                          : t('gallery.typeBody')}
              </span>
            </div>
          ))}
        </div>
      </Section>

      {SECTIONS.map(({ title, default: Specimens }) => (
        <Section key={title} title={title}>
          <Specimens />
        </Section>
      ))}
    </main>
  )
}

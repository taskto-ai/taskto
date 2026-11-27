import type { ReactNode } from 'react'
import './GalleryParts.css'

export interface GalleryModule {
  title: string
  default: () => ReactNode
}

export function Specimen({ caption, children }: { caption: ReactNode; children: ReactNode }) {
  return (
    <div className="ui-gallery-specimen">
      <p className="ui-gallery-caption">{caption}</p>
      <div className="ui-gallery-row">{children}</div>
    </div>
  )
}

export function SpecimenGrid({ children }: { children: ReactNode }) {
  return <div className="ui-gallery-grid">{children}</div>
}

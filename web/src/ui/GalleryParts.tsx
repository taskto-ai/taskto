// The pieces a component's gallery (<Name>.gallery.tsx) lays its specimens out with: a row of
// controls with a caption, and a grid of rows. Development only, like the gallery page.

import type { ReactNode } from 'react'
import './GalleryParts.css'

// A component's gallery file exports its title (the component's name, as code) and, as default,
// what it shows.
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

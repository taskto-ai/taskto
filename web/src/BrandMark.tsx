// The logo's mark, drawn from the taskto.ai website's own: an arrow (the task, in the text colour)
// handing off to a dot (the AI, in the node's green). Decorative: the name beside it says it.

export function BrandMark() {
  return (
    <svg className="tt-brand-mark" viewBox="0 0 64 64" aria-hidden="true" focusable="false">
      <g fill="none" stroke="currentColor" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round">
        <path d="M8 32h22" />
        <path d="M24 22l10 10-10 10" />
      </g>
      <circle className="tt-mark-dot" cx="51" cy="32" r="9" />
    </svg>
  )
}

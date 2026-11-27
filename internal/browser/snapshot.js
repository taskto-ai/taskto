(() => {
  const MAX = 48000
  const refs = new Map()
  let next = 0
  const live = (ref) => {
    const el = refs.get(ref)
    return el?.isConnected ? el : null
  }
  globalThis.__taskto = {
    rect(ref) {
      const el = live(ref)
      if (!el) return null
      el.scrollIntoView({ block: 'center', inline: 'center' })
      const r = el.getBoundingClientRect()
      if (r.width === 0 && r.height === 0) return null
      return { x: r.left + r.width / 2, y: r.top + r.height / 2 }
    },
    focus(ref) {
      const el = live(ref)
      if (!el) return false
      el.scrollIntoView({ block: 'center', inline: 'center' })
      el.focus()
      if (typeof el.select === 'function') {
        el.select()
      } else if (el.isContentEditable) {
        const range = document.createRange()
        range.selectNodeContents(el)
        const sel = getSelection()
        sel.removeAllRanges()
        sel.addRange(range)
      }
      const active = document.activeElement
      return active === el || el.contains(active)
    },
  }

  const SKIP = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'HEAD', 'META', 'LINK'])
  const ROLES = new Set([
    'button', 'link', 'checkbox', 'radio', 'tab', 'menuitem', 'menuitemcheckbox', 'menuitemradio',
    'option', 'switch', 'textbox', 'combobox', 'searchbox', 'slider', 'spinbutton', 'treeitem',
  ])
  const clean = (s, n = 80) => {
    const t = (s || '').replace(/\s+/g, ' ').trim()
    return t.length > n ? `${t.slice(0, n - 1)}…` : t
  }
  const visible = (el) =>
    typeof el.checkVisibility === 'function'
      ? el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true })
      : el.getClientRects().length > 0
  const interactive = (el) => {
    const tag = el.tagName
    if (tag === 'A') return el.hasAttribute('href')
    if (tag === 'INPUT') return el.type !== 'hidden'
    if (tag === 'BUTTON' || tag === 'SELECT' || tag === 'TEXTAREA' || tag === 'SUMMARY') return true
    if (el.isContentEditable && !el.parentElement?.isContentEditable) return true
    const role = el.getAttribute('role')
    if (role && ROLES.has(role)) return true
    return el.hasAttribute('onclick') || (el.tabIndex >= 0 && el.hasAttribute('tabindex'))
  }
  const labelOf = (el) => {
    const aria = el.getAttribute('aria-label')
    if (aria) return aria
    const by = el.getAttribute('aria-labelledby')
    if (by) {
      const text = by
        .split(/\s+/)
        .map((id) => document.getElementById(id)?.textContent || '')
        .join(' ')
      if (text.trim()) return text
    }
    if (el.labels?.length) return Array.from(el.labels, (l) => l.textContent).join(' ')
    return el.getAttribute('placeholder') || el.getAttribute('title') || el.getAttribute('alt') || ''
  }
  const describe = (el) => {
    const tag = el.tagName
    const role = el.getAttribute('role')
    let kind = role || tag.toLowerCase()
    let name = clean(labelOf(el))
    let extra = ''
    if (tag === 'A') {
      kind = role || 'link'
      name ||= clean(el.innerText)
      const href = el.getAttribute('href') || ''
      if (href && !href.startsWith('javascript:')) extra = ` -> ${clean(href, 120)}`
    } else if (tag === 'INPUT') {
      const type = el.type
      kind = type === 'checkbox' || type === 'radio' ? type : type === 'submit' || type === 'button' ? 'button' : `input ${type}`
      if (type === 'submit' || type === 'button') name ||= clean(el.value)
      if (type === 'checkbox' || type === 'radio') extra = el.checked ? ' (checked)' : ' (not checked)'
      else if (type === 'password') extra = el.value ? ' (filled)' : ' (empty)'
      else if (type !== 'submit' && type !== 'button') extra = ` = "${clean(el.value, 120)}"`
      name ||= clean(el.getAttribute('name'))
    } else if (tag === 'TEXTAREA') {
      kind = 'textarea'
      extra = ` = "${clean(el.value, 200)}"`
    } else if (tag === 'SELECT') {
      kind = 'select'
      const opts = Array.from(el.options)
      const chosen = opts.filter((o) => o.selected).map((o) => clean(o.text, 40))
      extra = ` = "${chosen.join(', ')}" (options: ${opts.slice(0, 15).map((o) => clean(o.text, 30)).join(' | ')}${opts.length > 15 ? ' | …' : ''})`
    } else {
      name ||= clean(el.innerText)
    }
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') extra += ' (disabled)'
    return `${kind}${name ? ` "${name}"` : ''}${extra}`
  }

  const lines = []
  let line = ''
  let size = 0
  let cut = false
  const flush = () => {
    const t = line.replace(/\s+/g, ' ').trim()
    line = ''
    if (!t || cut) return
    if (size + t.length > MAX) {
      cut = true
      return
    }
    lines.push(t)
    size += t.length + 1
  }
  const walk = (node) => {
    if (cut) return
    if (node.nodeType === Node.TEXT_NODE) {
      line += node.textContent
      return
    }
    if (node.nodeType !== Node.ELEMENT_NODE && node.nodeType !== Node.DOCUMENT_FRAGMENT_NODE) return
    if (node.nodeType === Node.ELEMENT_NODE) {
      const el = node
      if (SKIP.has(el.tagName) || !visible(el)) return
      if (el.tagName === 'IFRAME' || el.tagName === 'FRAME') {
        flush()
        line = `[frame] ${clean(el.title || el.getAttribute('name') || '', 60)} ${clean(el.src, 120)} (its content is not in this snapshot)`
        flush()
        return
      }
      if (interactive(el)) {
        flush()
        const ref = `e${++next}`
        refs.set(ref, el)
        line = `[${ref}] ${describe(el)}`
        flush()
        return
      }
      const display = getComputedStyle(el).display
      const block = !display.startsWith('inline') && display !== 'contents'
      if (block) flush()
      if (el.tagName === 'BR') flush()
      for (const child of el.childNodes) walk(child)
      if (el.shadowRoot) for (const child of el.shadowRoot.childNodes) walk(child)
      if (block) flush()
      return
    }
    for (const child of node.childNodes) walk(child)
  }
  if (document.body) walk(document.body)
  flush()
  const head = `Title: ${clean(document.title, 200)}\nURL: ${location.href}\n\n`
  return head + lines.join('\n') + (cut ? '\n… (the page goes on: this snapshot was cut)' : '')
})()

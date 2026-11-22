// The language the agent replies in: a system prompt section, from the language the server
// sends with every message (POST /conversations/:id/submit, `language`). The server picks it
// (auth.Service.Language, internal/auth/locale.go): the user's own pick, else their
// browser's, else their workspace's default. Without one, the agent replies in the language
// the user writes in.
//
// The section's text is English, whatever the language: models follow English instructions
// best, and the tools' names and descriptions are English too.

import { defineDoc, defineExtension, section } from '@earendil-works/pi-durable'

/** The conversation's language, a BCP 47 tag; '' for none known. A fork keeps it. */
export const Language = defineDoc({
  kind: 'taskto.language',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'current',
  initial: () => ({ tag: '' }),
  checkpointWhen: () => true,
})

// The tags the server stores: a language, a script and a region, in canonical case. The same
// pattern as internal/auth/locale.go's and web/src/i18n/index.tsx's; CI checks they agree
// (web/scripts/i18n-check.mjs).
const TAG = /^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$/

/** A language as the server sends them: a tag of TAG's form, or '' for none. */
export function validLanguage(value: unknown): value is string {
  return typeof value === 'string' && (value === '' || TAG.test(value))
}

/** The language's name in English and in itself: "Brazilian Portuguese (português (Brasil), pt-BR)". */
export function languageName(tag: string): string {
  const name = (locale: string) => {
    try {
      return new Intl.DisplayNames([locale], { type: 'language' }).of(tag)
    } catch {
      return undefined
    }
  }
  const english = name('en')
  const own = name(tag)
  if (!english || english === tag) return tag
  return own && own !== english ? `${english} (${own}, ${tag})` : `${english} (${tag})`
}

/** The section's text for tag, or for no known language when tag is ''. */
export function languageInstructions(tag: string): string {
  const content =
    'Write what you make for the user (documents, cards, apps and their text) in that language too, unless they ask for another or add to something already written in another. Names of tools, fields and files stay as they are.'
  if (!tag) return `Reply in the language the user writes in. ${content}`
  return [
    `The user's language is ${languageName(tag)}. Reply in it, unless the user writes to you in another language: then reply in theirs.`,
    content,
  ].join(' ')
}

export const LanguageSection = defineExtension({
  name: 'taskto-language',
  sections: [
    section('language', async (input, context) => {
      const doc = await input.read.snapshot(Language, input.conversationId, context)
      return languageInstructions(doc?.tag ?? '')
    }),
  ],
})

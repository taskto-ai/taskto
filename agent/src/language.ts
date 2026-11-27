import { defineDoc, defineExtension, section } from '@earendil-works/pi-durable'

export const Language = defineDoc({
  kind: 'taskto.language',
  version: 1,
  scope: 'conversation',
  history: 'latest',
  fork: 'current',
  initial: () => ({ tag: '' }),
  checkpointWhen: () => true,
})

const TAG = /^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$/

export function validLanguage(value: unknown): value is string {
  return typeof value === 'string' && (value === '' || TAG.test(value))
}

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

import { describe, expect, it } from 'vitest'
import { languageInstructions, languageName, validLanguage } from './language.js'

describe('validLanguage', () => {
  it.each(['', 'en', 'pt', 'pt-BR', 'zh-Hant', 'zh-Hant-TW', 'es-419', 'fil'])('accepts %j', (tag) => {
    expect(validLanguage(tag)).toBe(true)
  })

  it.each(['EN', 'en_US', 'pt-br', 'e', 'english', 'en-', 'en-US-x', ' en', 'en\n', 'en-US\u0000', '<script>'])(
    'refuses %j',
    (tag) => {
      expect(validLanguage(tag)).toBe(false)
    },
  )

  it.each([undefined, null, 5, {}, ['en']])('refuses a value that is not a string: %j', (value) => {
    expect(validLanguage(value)).toBe(false)
  })
})

describe('languageName', () => {
  it("gives the English name and the language's own", () => {
    expect(languageName('pt-BR')).toMatch(/^Brazilian Portuguese \(.+, pt-BR\)$/)
    expect(languageName('de')).toMatch(/^German \(Deutsch, de\)$/)
  })

  it('names English once', () => {
    expect(languageName('en')).toBe('English (en)')
  })

  it('falls back to the tag when no name is known', () => {
    expect(languageName('zz')).toBe('zz')
  })
})

describe('languageInstructions', () => {
  it('follows the language the user writes in when none is known', () => {
    const text = languageInstructions('')
    expect(text).toMatch(/^Reply in the language the user writes in\./)
    expect(text).toContain('Names of tools, fields and files stay as they are.')
  })

  it('names the language and still lets the user write in another', () => {
    const text = languageInstructions('pt-BR')
    expect(text).toContain("The user's language is Brazilian Portuguese")
    expect(text).toContain('pt-BR')
    expect(text).toContain('unless the user writes to you in another language')
  })

  it('is English whatever the language', () => {
    expect(languageInstructions('ja')).toMatch(/^The user's language is Japanese/)
  })
})

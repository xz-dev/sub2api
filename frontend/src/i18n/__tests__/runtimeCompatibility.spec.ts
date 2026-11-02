import { afterEach, describe, expect, it, vi } from 'vitest'

afterEach(() => {
  localStorage.removeItem('sub2api_locale')
  document.documentElement.removeAttribute('lang')
  vi.resetModules()
})

describe('application i18n runtime compatibility', () => {
  it.each([
    ['en', 'Save', '(3 selected)'],
    ['zh', '保存', '（已选 3 个）']
  ])('loads the saved %s locale and interpolates application messages', async (locale, save, selected) => {
    localStorage.setItem('sub2api_locale', locale)
    const { getLocale, i18n, initI18n } = await import('../index')

    await initI18n()

    expect(getLocale()).toBe(locale)
    expect(document.documentElement.lang).toBe(locale)
    expect(i18n.global.t('common.save')).toBe(save)
    expect(i18n.global.t('common.selectedCount', { count: 3 })).toBe(selected)
  })

  it('keeps loaded-locale switching and English fallback interpolation', async () => {
    localStorage.setItem('sub2api_locale', 'en')
    const { getLocale, i18n, initI18n, loadLocaleMessages } = await import('../index')
    await initI18n()
    await loadLocaleMessages('zh')
    i18n.global.mergeLocaleMessage('en', { compatibility: { fallback: 'Fallback {count}' } })

    i18n.global.locale.value = 'zh'

    expect(getLocale()).toBe('zh')
    expect(i18n.global.t('common.save')).toBe('保存')
    expect(i18n.global.t('compatibility.fallback', { count: 3 })).toBe('Fallback 3')

    i18n.global.locale.value = 'en'
    expect(i18n.global.t('common.save')).toBe('Save')
  })
})

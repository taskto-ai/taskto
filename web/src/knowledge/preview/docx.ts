export async function renderDocx(bytes: ArrayBuffer, into: HTMLElement): Promise<void> {
  const { renderAsync } = await import('docx-preview')
  await renderAsync(bytes, into, into, {
    inWrapper: true,
    ignoreLastRenderedPageBreak: true,
    experimental: false,
    useBase64URL: true,
  })
}

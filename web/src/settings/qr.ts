// Draws text as a QR code, in the browser, for the authenticator app's secret (docs/api.md#two-factor-login):
// the secret is never sent anywhere to be drawn. It is the writer half of zxing-wasm (MIT; its .wasm is
// ZXing-C++, Apache-2.0), a package the app already ships for scanning barcodes (apps/zxing.ts), loaded
// the first time it is needed and from the app's own files, never from a CDN. The service worker
// doesn't precache the writer's .wasm (pwa/plugin.ts): turning two-factor login on needs the server anyway.

export async function qrSvg(text: string): Promise<string> {
  const [writer, wasm] = await Promise.all([
    import('zxing-wasm/writer'),
    import('zxing-wasm/writer/zxing_writer.wasm?url'),
  ])
  writer.setZXingModuleOverrides({
    locateFile: (path: string, prefix: string) => (path.endsWith('.wasm') ? wasm.default : prefix + path),
  })
  const out = await writer.writeBarcode(text, { format: 'QRCode', ecLevel: 'M', withQuietZones: true })
  if (out.error || !out.svg) throw new Error(out.error || 'no QR code was made')
  return out.svg
}

// An <img> source for the drawing. The SVG is never put in the page as markup.
export function svgSource(svg: string): string {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

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

export function svgSource(svg: string): string {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`
}

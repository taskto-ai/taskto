// The bundled barcode decoder, loaded on demand by scanner.ts only where the browser has no
// BarcodeDetector of its own: zxing-wasm (MIT; its .wasm is ZXing-C++, Apache-2.0, compiled to
// WebAssembly) behind the BarcodeDetector API, by the barcode-detector package (MIT). The licences'
// notices ship with the build as assets/THIRD_PARTY_NOTICES.txt (pwa/notices.ts). The package would fetch its
// .wasm file from a CDN by default; it is pointed at the copy the build ships beside the app
// instead, so a scan never asks an outside server for anything.

import { BarcodeDetector, setZXingModuleOverrides } from 'barcode-detector/ponyfill'
import wasmUrl from 'zxing-wasm/reader/zxing_reader.wasm?url'

setZXingModuleOverrides({
  locateFile: (path: string, prefix: string) => (path.endsWith('.wasm') ? wasmUrl : prefix + path),
})

export { BarcodeDetector }

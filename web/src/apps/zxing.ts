import { BarcodeDetector, setZXingModuleOverrides } from 'barcode-detector/ponyfill'
import wasmUrl from 'zxing-wasm/reader/zxing_reader.wasm?url'

setZXingModuleOverrides({
  locateFile: (path: string, prefix: string) => (path.endsWith('.wasm') ? wasmUrl : prefix + path),
})

export { BarcodeDetector }

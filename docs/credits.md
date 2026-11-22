# Credits

TaskTo.AI is MIT-licensed. This page lists third-party work it downloads or runs that asks
for attribution or notices. Nothing listed here is in this repository or the container
image: each is downloaded on first use, pinned by sha256 (see
[install.md](install.md#dictation)), or installed by the operator. The web app's About
page (Settings → About) shows the credits of whatever a workspace uses.

## Dictation, local models

- **Parakeet TDT 0.6B v3** by NVIDIA, licensed under
  [CC-BY-4.0](https://creativecommons.org/licenses/by/4.0/):
  <https://huggingface.co/nvidia/parakeet-tdt-0.6b-v3>. TaskTo.AI uses the int8 ONNX export
  that the sherpa-onnx project publishes
  (<https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8>). That
  export is quantized; the model is not otherwise changed. Used when `dictation_local` is
  `parakeet-v3`, the default.
- **sherpa-onnx** by the k2-fsa project, Apache-2.0: <https://github.com/k2-fsa/sherpa-onnx>.
  Its release builds include **ONNX Runtime** by Microsoft, MIT:
  <https://github.com/microsoft/onnxruntime>.
- **Whistle** by Cactus Compute, Apache-2.0: <https://huggingface.co/Cactus-Compute/whistle>.
  Used when `dictation_local` is `whistle`.
- **Cactus runtime 4.0** by Cactus Compute, Apache-2.0, binaries included:
  <https://github.com/cactus-compute/cactus>.

## Add-ons the operator installs

- **Voz** by Desert Ant Labs, under the
  [Desert Ant Labs Source-Available License 1.1](https://license.desertant.com/). It is
  never bundled with TaskTo.AI. While a workspace uses it, the About page shows "Speech
  recognition: Voz, powered by Desert Ant Labs", linked to <https://desertant.com>.

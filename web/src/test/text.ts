export function wholeText(text: string) {
  return (_: string, el: Element | null) =>
    el?.textContent === text && ![...el.children].some((child) => child.textContent === text)
}

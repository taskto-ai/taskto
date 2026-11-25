// A text matcher for text that is split over elements (a tool's name in the mono face inside its
// line): the innermost element whose text is all of it.
export function wholeText(text: string) {
  return (_: string, el: Element | null) =>
    el?.textContent === text && ![...el.children].some((child) => child.textContent === text)
}

// Which signed-in session the page is in now: App counts one more each time the account changes or
// signs out (logged out, the session lost, another account signed in). Work a session started
// (a save, a create) takes `sameSession()` when it starts and asks it when its answer comes: one
// that comes after the session ended writes nothing and says nothing, as the browser may be the
// next person's by then.
//
// It is apart from whether the screen that started it is still mounted (mounted.ts): the shell
// can go and come back within one session (opening a push's notification), and what belongs to the
// account (a language being saved, a workspace made) goes on through that.

let generation = 0

export function sessionChanged(): void {
  generation += 1
}

export function sameSession(): () => boolean {
  const at = generation
  return () => generation === at
}

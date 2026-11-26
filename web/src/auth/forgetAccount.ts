import { deactivate } from '../offline/runtime'
import { approvalOf } from '../offline/store'
import { forgetDrafts } from '../uiState'

// Removes everything this device kept of user's account: the drafts and the offline database,
// whatever it still holds unsent (the account is gone, so there is no one to send it to). It is
// done by the page that deleted the account, from the server's answer to it, and by nothing that
// listens for the session ending: a request that was refused a moment earlier ends the session in
// the app first, and the answer to the deletion would then find no one listening.
export async function forgetAccount(user: number): Promise<void> {
  forgetDrafts()
  try {
    let left = await deactivate({ purge: true, user })
    // Deleting is refused while records that weren't shown to the person are unsent: they are
    // shown now, by being named, and the account's deletion is what approves them.
    if (left.length > 0) left = await deactivate({ purge: true, user, approved: approvalOf(left) })
  } catch {
    // The database can't be opened (a newer version's, no storage): nothing more can be done here.
  }
}

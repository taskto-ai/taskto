import { browserLogin, forgetLogin } from '../offline/pendingSignOut'
import { purgeAccount } from '../offline/runtime'
import { approvalOf } from '../offline/store'
import { forgetDraftsOf } from '../uiState'

export async function forgetAccount(user: number): Promise<void> {
  const login = browserLogin()
  if (login?.user === user) forgetLogin(user, login.session)
  forgetDraftsOf(user)
  try {
    let left = await purgeAccount(user)
    if (left.length > 0) left = await purgeAccount(user, approvalOf(left))
  } catch {}
}

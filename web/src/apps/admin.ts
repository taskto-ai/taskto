// Whether the signed-in person is an admin of the workspace the Apps screens are showing. The shell
// says it once, around the section; screens that offer an admin's step ("approve and publish") ask
// here instead of every screen between passing it down. It only decides what is offered: the
// server checks the role on every admin route, and a member who got the step would be refused.

import { createContext, useContext } from 'react'

const Admin = createContext(false)

export const WorkspaceAdmin = Admin.Provider

export function useIsAdmin(): boolean {
  return useContext(Admin)
}

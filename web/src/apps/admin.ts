import { createContext, useContext } from 'react'

const Admin = createContext(false)

export const WorkspaceAdmin = Admin.Provider

export function useIsAdmin(): boolean {
  return useContext(Admin)
}

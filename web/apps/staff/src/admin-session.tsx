import { ApiError, communityStaffApi, type CommunityStaffTypes } from "@fairlb/api-client";
import { createContext, useContext, type ReactNode } from "react";

export type AdminIdentity = CommunityStaffTypes.CommunityIdentity;

const IdentityContext = createContext<AdminIdentity | null>(null);

export function AdminIdentityProvider({
  value,
  children,
}: {
  value: AdminIdentity;
  children: ReactNode;
}) {
  return <IdentityContext.Provider value={value}>{children}</IdentityContext.Provider>;
}

export function useCurrentAdmin(): AdminIdentity {
  const identity = useContext(IdentityContext);
  if (!identity) throw new Error("useCurrentAdmin must be used inside RequireAdmin");
  return identity;
}

export function useAdminMe() {
  return communityStaffApi.useCommunityMe({
    query: {
      retry: (count, error) => !(error instanceof ApiError && error.status === 401) && count < 2,
      staleTime: 30_000,
    },
  });
}

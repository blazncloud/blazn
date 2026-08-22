import type { ApiErrorCode } from "./contract.js";

export interface SessionAccessState {
  sessionRevokedAt: Date | null;
  deviceRevokedAt: Date | null;
  accessExpiresAt: Date;
}

export function sessionAccessError(state: SessionAccessState, now = Date.now()): ApiErrorCode | undefined {
  if (state.deviceRevokedAt) return "device_revoked";
  if (state.sessionRevokedAt) return "session_revoked";
  if (state.accessExpiresAt.getTime() <= now) return "access_expired";
  return undefined;
}

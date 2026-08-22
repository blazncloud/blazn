import { HttpError } from "./http.js";
import { NodeHttpError } from "./node-types.js";
import { ProjectHttpError } from "./project-types.js";
import { SandboxHttpError } from "./sandbox-types.js";
import { RunHttpError } from "./run-types.js";
import { WorkspaceHttpError } from "./workspace-types.js";

export type ControlHttpError = HttpError | WorkspaceHttpError | NodeHttpError | ProjectHttpError | SandboxHttpError | RunHttpError;

export function isControlHttpError(error: unknown): error is ControlHttpError {
  return error instanceof HttpError
    || error instanceof WorkspaceHttpError
    || error instanceof NodeHttpError
    || error instanceof ProjectHttpError
    || error instanceof RunHttpError
    || error instanceof SandboxHttpError;
}

export function normalizeControlHttpError(error: unknown): ControlHttpError {
  return isControlHttpError(error) ? error : new HttpError("internal_error", "request failed");
}

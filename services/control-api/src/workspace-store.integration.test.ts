import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import test from "node:test";
import pg from "pg";
import { PgWorkspaceStore, type WorkspaceStore, type WorkspaceTransaction } from "./workspace-store.js";
import { WorkspaceService } from "./workspace-service.js";
import { WorkspaceHttpError, type WorkspacePrincipal } from "./workspace-types.js";

const databaseUrl = process.env.BLAZN_WORKSPACE_TEST_DATABASE_URL;
const adminDatabaseUrl = process.env.BLAZN_WORKSPACE_TEST_ADMIN_DATABASE_URL ?? databaseUrl;

test("PostgreSQL workspace isolation, idempotency, invitation race, owner safety, and removal", { skip: !databaseUrl }, async () => {
  const database = new pg.Pool({ connectionString: databaseUrl, max: 12 });
  const admin = new pg.Pool({ connectionString: adminDatabaseUrl, max: 2 });
  const owner = principal(); const member = principal(); const rival = principal();
  try {
    await admin.query("TRUNCATE workspace_audit_events,workspace_idempotency_receipts,workspace_invitations,workspace_memberships,workspaces,users CASCADE");
    for (const user of [owner, member, rival]) await admin.query("INSERT INTO users(id,email,display_name,password_salt,password_hash) VALUES($1,$2,$3,'salt','hash')", [user.userId, user.email, user.displayName]);
    const service = new WorkspaceService(new PgWorkspaceStore(database), async () => Buffer.alloc(32, 3));

    const created = await service.createWorkspace(owner, "create-key-0001", { name: "Acme", slug: "acme" });
    const repeated = await service.createWorkspace(owner, "create-key-0001", { name: "Acme", slug: "acme" });
    assert.equal(repeated.workspace.id, created.workspace.id);
    await assert.rejects(service.createWorkspace(owner, "create-key-0001", { name: "Changed", slug: "changed" }), isCode("idempotency_conflict"));
    const other = await service.createWorkspace(rival, "create-key-0002", { name: "Other", slug: "other" });
    await assert.rejects(service.getWorkspace(member, other.workspace.id), isCode("workspace_not_found"));

    const invite = await service.createInvitation(owner, created.workspace.id, "invite-key-0001", { role: "member", expiresIn: 600 });
    const inviteReplay = await service.createInvitation(owner, created.workspace.id, "invite-key-0001", { role: "member", expiresIn: 600 });
    assert.equal(inviteReplay.inviteToken, invite.inviteToken);
    const results = await Promise.allSettled([
      service.acceptInvitation(member, "accept-key-0001", invite.inviteToken),
      service.acceptInvitation(rival, "accept-key-0002", invite.inviteToken),
    ]);
    assert.equal(results.filter((result) => result.status === "fulfilled").length, 1);
    const joined = results[0]?.status === "fulfilled" ? member : rival;
    const rejected = joined.userId === member.userId ? rival : member;
    await assert.rejects(service.acceptInvitation(rejected, "accept-key-0003", invite.inviteToken), isCode("invitation_consumed"));
    await assert.rejects(service.createInvitation(joined, created.workspace.id, "invite-key-0004", { role: "member", expiresIn: 600 }), isCode("permission_denied"));

    const ownerMembership = (await service.listMembers(owner, created.workspace.id)).items.find((item) => item.user.id === owner.userId)!;
    await assert.rejects(service.removeMember(owner, created.workspace.id, owner.userId, ownerMembership.version, "remove-key-0001"), isCode("last_owner"));
    const joinedMembership = (await service.listMembers(owner, created.workspace.id)).items.find((item) => item.user.id === joined.userId)!;
    const eventRead = deferred<void>();
    const releaseEventRead = deferred<void>();
    const serializedService = new WorkspaceService(new PausingEventStore(database, eventRead.resolve, releaseEventRead.promise), async () => Buffer.alloc(32, 3));
    const batch = serializedService.eventBatch(joined, created.workspace.id);
    await eventRead.promise;
    const removal = service.removeMember(owner, created.workspace.id, joined.userId, joinedMembership.version, "remove-key-0002");
    const removedBeforeReadCommitted = await Promise.race([removal.then(() => true), new Promise<false>((resolve) => setTimeout(() => resolve(false), 100))]);
    assert.equal(removedBeforeReadCommitted, false, "membership removal bypassed the event authority lock");
    const stillActive = await admin.query<{ status: string }>("SELECT status FROM workspace_memberships WHERE workspace_id=$1 AND user_id=$2", [created.workspace.id, joined.userId]);
    assert.equal(stillActive.rows[0]?.status, "active");
    releaseEventRead.resolve();
    await batch;
    await removal;
    await assert.rejects(service.getWorkspace(joined, created.workspace.id), isCode("workspace_not_found"));
    await assert.rejects(service.eventBatch(joined, created.workspace.id), isCode("workspace_not_found"));
  } finally {
    await database.end();
    await admin.end();
  }
});

function principal(): WorkspacePrincipal {
  const id = randomUUID();
  return { userId: id, email: `${id}@example.test`, displayName: id.slice(0, 8) };
}
function isCode(code: string): (error: unknown) => boolean {
  return (error) => error instanceof WorkspaceHttpError && error.code === code;
}

class PausingEventStore implements WorkspaceStore {
  private readonly delegate: PgWorkspaceStore;
  private pause = true;

  constructor(database: ConstructorParameters<typeof PgWorkspaceStore>[0], private readonly entered: () => void, private readonly release: Promise<void>) {
    this.delegate = new PgWorkspaceStore(database);
  }

  transaction<T>(action: (transaction: WorkspaceTransaction) => Promise<T>): Promise<T> {
    return this.delegate.transaction((transaction) => action(new Proxy(transaction, {
      get: (target, property) => {
        if (property === "listEvents") return async (workspaceId: string, afterId = "") => {
          if (this.pause) {
            this.pause = false;
            this.entered();
            await this.release;
          }
          return target.listEvents(workspaceId, afterId);
        };
        const value = Reflect.get(target, property);
        return typeof value === "function" ? value.bind(target) : value;
      },
    })));
  }

  listWorkspaces(...args: Parameters<PgWorkspaceStore["listWorkspaces"]>) { return this.delegate.listWorkspaces(...args); }
  getWorkspace(...args: Parameters<PgWorkspaceStore["getWorkspace"]>) { return this.delegate.getWorkspace(...args); }
  listInvitations(...args: Parameters<PgWorkspaceStore["listInvitations"]>) { return this.delegate.listInvitations(...args); }
  listMembers(...args: Parameters<PgWorkspaceStore["listMembers"]>) { return this.delegate.listMembers(...args); }
  listEvents(...args: Parameters<PgWorkspaceStore["listEvents"]>) { return this.delegate.listEvents(...args); }
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

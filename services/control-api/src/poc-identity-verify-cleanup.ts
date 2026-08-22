import { readFile } from "node:fs/promises";
import { createDatabase } from "./db.js";
import { assertPocIdentityAbsent, type CleanupIntent } from "./poc-cleanup-state.js";

const databaseFile = process.env.POC_IDENTITY_DATABASE_URL_FILE;
const intentFile = process.env.POC_IDENTITY_CLEANUP_INTENT_FILE;
if (!databaseFile || !intentFile) throw new Error("POC cleanup verification database and intent files are required");
const intent = JSON.parse(await readFile(intentFile, "utf8")) as CleanupIntent;
const database = createDatabase((await readFile(databaseFile, "utf8")).trim());
try {
  await assertPocIdentityAbsent(database, intent);
  process.stdout.write(JSON.stringify({ status: "absent", userId: intent.userId }) + "\n");
} finally {
  await database.end();
}

import pg from "pg";

const { Pool } = pg;

export type Database = InstanceType<typeof Pool>;

export function createDatabase(connectionString: string): Database {
  return new Pool({ connectionString, max: 10, idleTimeoutMillis: 30_000 });
}

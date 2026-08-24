import { readdir } from "node:fs/promises";

const migrationPattern = /^(\d{3})_[a-z0-9][a-z0-9_]*\.sql$/;

export function validateMigrationInventory(names: readonly string[]): string[] {
  if (names.length === 0) throw new Error("migration inventory is empty");

  const migrations = [...names];
  const ordered = [...migrations].sort();
  if (migrations.some((name, index) => name !== ordered[index])) {
    throw new Error("migration inventory is out of order");
  }

  const seenNames = new Set<string>();
  const seenRevisions = new Set<number>();
  for (const [index, name] of migrations.entries()) {
    const match = migrationPattern.exec(name);
    if (!match) throw new Error(`invalid migration filename ${name}`);
    if (seenNames.has(name)) throw new Error(`duplicate migration ${name}`);
    seenNames.add(name);

    const revision = Number(match[1]);
    if (seenRevisions.has(revision)) throw new Error(`duplicate migration revision ${match[1]}`);
    seenRevisions.add(revision);

    const expected = index + 1;
    if (revision !== expected) {
      throw new Error(`missing migration revision ${String(expected).padStart(3, "0")}`);
    }
  }
  return migrations;
}

export async function readMigrationInventory(directory: string): Promise<string[]> {
  const names = (await readdir(directory)).filter((name) => name.endsWith(".sql")).sort();
  return validateMigrationInventory(names);
}

export function validateAppliedMigrations(
  inventory: readonly string[],
  applied: readonly string[],
  requireComplete = false,
): void {
  const seen = new Set<string>();
  for (const [index, name] of applied.entries()) {
    if (seen.has(name)) throw new Error(`duplicate applied migration ${name}`);
    seen.add(name);
    const expected = inventory[index];
    if (name !== expected) {
      if (inventory.includes(name)) throw new Error(`applied migration ${name} is out of order; expected ${expected ?? "none"}`);
      throw new Error(`applied migration ${name} is absent from the migration inventory`);
    }
  }
  if (requireComplete && applied.length !== inventory.length) {
    throw new Error(`unapplied migration ${inventory[applied.length] ?? "unknown"}`);
  }
}

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
  optionalLegacy: readonly string[] = [],
): void {
  const legacy = new Set(optionalLegacy);
  for (const name of legacy) {
    if (!migrationPattern.test(name)) throw new Error(`invalid legacy migration filename ${name}`);
    if (inventory.includes(name)) throw new Error(`legacy migration ${name} is in the active inventory`);
  }

  const seen = new Set<string>();
  let inventoryIndex = 0;
  let previous: string | undefined;
  for (const name of applied) {
    if (seen.has(name)) throw new Error(`duplicate applied migration ${name}`);
    if (previous !== undefined && name <= previous) throw new Error(`applied migration ${name} is out of order`);
    seen.add(name);
    previous = name;
    const expected = inventory[inventoryIndex];
    if (name === expected) {
      inventoryIndex += 1;
      continue;
    }
    if (legacy.has(name)) {
      const priorActive = inventoryIndex === 0 ? undefined : inventory[inventoryIndex - 1];
      if ((priorActive !== undefined && name <= priorActive) || (expected !== undefined && name >= expected)) {
        throw new Error(`legacy applied migration ${name} is out of order; expected ${expected ?? "none"}`);
      }
      continue;
    }
    if (inventory.includes(name)) throw new Error(`applied migration ${name} is out of order; expected ${expected ?? "none"}`);
    throw new Error(`applied migration ${name} is absent from the migration inventory`);
  }
  if (requireComplete && inventoryIndex !== inventory.length) {
    throw new Error(`unapplied migration ${inventory[inventoryIndex] ?? "unknown"}`);
  }
}

export function validateAppliedMigrationChecksums(
  expected: ReadonlyMap<string, string>,
  applied: readonly { version: string; checksum: string }[],
): void {
  for (const row of applied) {
    const checksum = expected.get(row.version);
    if (checksum !== undefined && row.checksum !== checksum) {
      throw new Error(`applied migration ${row.version} has changed`);
    }
  }
}

import assert from "node:assert/strict";
import test from "node:test";
import { validateAppliedMigrationChecksums, validateAppliedMigrations, validateMigrationInventory } from "./migration-inventory.js";

const inventory = ["001_auth.sql", "002_workspaces.sql", "003_projects.sql"] as const;

test("migration inventory accepts one ordered collision-free sequence", () => {
  assert.deepEqual(validateMigrationInventory(inventory), inventory);
});

test("migration inventory rejects a missing revision", () => {
  assert.throws(() => validateMigrationInventory(["001_auth.sql", "003_projects.sql"]), /missing migration revision 002/);
});

test("migration inventory rejects duplicate revisions", () => {
  assert.throws(
    () => validateMigrationInventory(["001_auth.sql", "002_a.sql", "002_b.sql"]),
    /duplicate migration revision 002/,
  );
});

test("migration inventory rejects out-of-order files", () => {
  assert.throws(
    () => validateMigrationInventory(["002_workspaces.sql", "001_auth.sql", "003_projects.sql"]),
    /out of order/,
  );
});

test("applied migration validation rejects an unapplied inventory suffix", () => {
  assert.throws(() => validateAppliedMigrations(inventory, inventory.slice(0, 2), true), /unapplied migration 003_projects.sql/);
});

test("applied migration validation permits only an ordered prefix during migration", () => {
  assert.doesNotThrow(() => validateAppliedMigrations(inventory, inventory.slice(0, 2)));
  assert.throws(
    () => validateAppliedMigrations(inventory, ["001_auth.sql", "003_projects.sql"]),
    /003_projects.sql is out of order; expected 002_workspaces.sql/,
  );
});

test("applied migration validation accepts an optional historical migration in its lexical slot", () => {
  const legacy = ["003_password_recovery.sql"] as const;
  assert.doesNotThrow(() => validateAppliedMigrations(inventory, ["001_auth.sql", "002_workspaces.sql", legacy[0]], false, legacy));
  assert.doesNotThrow(() => validateAppliedMigrations(inventory, [...inventory.slice(0, 2), legacy[0], inventory[2]], true, legacy));
  assert.doesNotThrow(() => validateAppliedMigrations(inventory, inventory, true, legacy));
});

test("applied migration validation rejects misplaced or unknown historical migrations", () => {
  const legacy = ["003_password_recovery.sql"] as const;
  assert.throws(
    () => validateAppliedMigrations(inventory, ["001_auth.sql", legacy[0]], false, legacy),
    /legacy applied migration .* is out of order/,
  );
  assert.throws(
    () => validateAppliedMigrations(inventory, ["001_auth.sql", "002_workspaces.sql", "003_unknown.sql"]),
    /absent from the migration inventory/,
  );
});

test("historical migration checksum validation fails closed", () => {
  const expected = new Map([["003_password_recovery.sql", "abc"]]);
  assert.doesNotThrow(() => validateAppliedMigrationChecksums(expected, [{ version: "003_password_recovery.sql", checksum: "abc" }]));
  assert.throws(
    () => validateAppliedMigrationChecksums(expected, [{ version: "003_password_recovery.sql", checksum: "def" }]),
    /applied migration 003_password_recovery.sql has changed/,
  );
});

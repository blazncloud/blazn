import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

export function digest(value) {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

export function solveTask(task, source) {
  if (task.schemaVersion !== "blazn.dev/coding-task/v1alpha1") {
    throw new Error("unsupported coding task contract");
  }
  if (digest(source) !== task.sourceDigest) {
    throw new Error("source digest does not match the immutable task");
  }
  const first = source.indexOf(task.find);
  if (first < 0 || source.indexOf(task.find, first + task.find.length) >= 0) {
    throw new Error("task replacement must match exactly once");
  }
  return `${source.slice(0, first)}${task.replace}${source.slice(first + task.find.length)}`;
}

async function main(argv) {
  if (argv.length !== 4 || argv[0] !== "--task" || argv[2] !== "--source") {
    throw new Error("usage: coding-agent --task TASK_JSON --source SOURCE_FILE");
  }
  const task = JSON.parse(await readFile(argv[1], "utf8"));
  const source = await readFile(argv[3], "utf8");
  process.stdout.write(solveTask(task, source));
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}

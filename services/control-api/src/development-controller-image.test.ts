import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("Development controller image pins BuildKit and Node and remains unprivileged",async()=>{const dockerfile=await readFile(new URL("../Dockerfile.development-controller",import.meta.url),"utf8");assert.match(dockerfile,/FROM moby\/buildkit@sha256:79cc6476ab1a3371c9afd8b44e7c55610057c43e18d9b39b68e2b0c2475cc1b6 AS buildkit/);assert.equal((dockerfile.match(/FROM node:22\.19\.0-bookworm-slim@sha256:4a4884e8a44826194dff92ba316264f392056cbe243dcc9fd3551e71cea02b90/g)??[]).length,2);assert.match(dockerfile,/COPY --from=buildkit \/usr\/bin\/buildctl \/usr\/local\/bin\/buildctl/);assert.match(dockerfile,/USER node\s+ENTRYPOINT \["node", "dist\/development-controller-main\.js"\]/);assert.doesNotMatch(dockerfile,/:latest|apt-get|curl|docker\.sock/);});

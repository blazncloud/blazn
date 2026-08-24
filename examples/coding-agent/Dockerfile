FROM docker.io/library/node@sha256:d2166de198f26e17e5a442f537754dd616ab069c47cc57b889310a717e0abbf9 AS verify

WORKDIR /workspace/src/blazn/examples/coding-agent
COPY package.json package-lock.json ./
RUN ["npm", "ci", "--ignore-scripts", "--offline", "--no-audit", "--no-fund"]
COPY src ./src
COPY test ./test
COPY scripts/context-identity.mjs ./scripts/context-identity.mjs
COPY fixtures/task.json ./fixtures/task.json
COPY fixtures/source ./fixtures/source
RUN ["node", "--test", "test/coding-agent.test.mjs"]

FROM docker.io/library/node@sha256:d2166de198f26e17e5a442f537754dd616ab069c47cc57b889310a717e0abbf9
WORKDIR /workspace
ENV NODE_ENV=production
COPY --from=verify --chown=1000:1000 /workspace/src/blazn/examples/coding-agent /opt/coding-agent
WORKDIR /opt/coding-agent
USER 1000:1000
ENTRYPOINT ["node", "src/coding-agent.mjs"]
CMD ["--task", "/workspace/src/blazn/examples/coding-agent/fixtures/task.json", "--source-root", "/workspace/src/blazn", "--output", "/workspace/artifacts/change.patch"]

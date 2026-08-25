#!/usr/local/bin/node
const collector=await import("/app/dist/development-evidence-collector-main.js");
await collector.mainDevelopmentEvidenceCollector();

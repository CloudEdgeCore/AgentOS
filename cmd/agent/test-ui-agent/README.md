# test-ui-agent Agent Project

Scaffolded by AgentOS CLI (agent init).

## Project Layout
- agent.yaml         : Project-level configuration (cascades over global settings)
- agent.manifest.json: Capability, tool permissions, and budget declaration
- prompt.md          : System prompt and output schema
- tools/             : Custom tool service source code

## Run
$ agent run test-ui-agent/agent.manifest.json

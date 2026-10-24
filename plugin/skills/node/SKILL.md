---
name: node
description: Add a local inference node (Ollama, vLLM, llama.cpp on a Mac or a GPU box) to Keystone (KTMS) and route to it with cloud fallback. Use when asked to run models locally, add a GPU machine or keep traffic on private hardware.
---

# Add a local node

## Steps

1. What is there: `ktms nodes list` and `ktms models list` (MCP `list_nodes`, `list_models`).
2. Does the model fit? Weights plus about 1.5 GB plus about 1 GB per 8192 context tokens, against
   about 3/4 of RAM on a Mac or the GPU's memory on Linux. If not, suggest the largest that fits.
3. A node key bound to one node id, shown once: `ktms node-keys mint NAME --node-id ID`.
4. The model as vendor `local`, id exactly the engine's model tag, with the efforts it may serve,
   and a rule that puts it first with cloud models after it. Add both to `ktms.yaml` and
   plan and apply (see the configure skill); keep a catch-all rule per effort.
5. On the machine, the installer sets up the engine, the model and the agent and waits for
   registration (it asks for the node key with hidden input):
   `curl -fsSL https://raw.githubusercontent.com/ai-process/ktms/main/scripts/install-node.sh | sh -s -- --proxy GATEWAY --node-id ID --model TAG`
   `--doctor` explains failures, `--dry-run` changes nothing, `--uninstall` removes it.

## Check

- `ktms nodes list` shows the node `HEALTHY` with the model.
- `ktms route explain KEY --effort low --placement nodes_only` keeps the local model; if it says
  `node_unavailable`, the node is not serving it yet.
- `ktms chat --model auto:nodes-only:low "hello"` reports vendor `local`; with the node
  stopped, a normal call falls back to the cloud (`ktms nodes drain ID` to stop sending it work).

Sources: [Run a node](https://github.com/ai-process/ktms/blob/main/README.md#run-a-node), [If the node does not show up](https://github.com/ai-process/ktms/blob/main/README.md#if-the-node-does-not-show-up),
[Choosing a model and placement](https://github.com/ai-process/ktms/blob/main/README.md#choosing-a-model-and-placement).

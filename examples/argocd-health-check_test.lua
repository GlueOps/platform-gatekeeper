-- Tests for the Lua health check in argocd-health-check.yaml.
--
-- This script is load-bearing and fails silently when wrong: Argo CD ignores a
-- resource whose health check errors, so the wave hangs with the Application
-- showing Unknown and nothing alerting. The generation guard is the sharpest part —
-- without it an edited Gate is judged against the condition left from its previous
-- spec, so Argo CD sees Healthy and proceeds, bypassing the gate on the very sync
-- that tightened it.
--
-- Run it (the script lives inside a ConfigMap value, so extract it first):
--
--   mkdir -p .luatest && cp examples/argocd-health-check_test.lua .luatest/
--   docker run --rm -v "$PWD":/w -w /w python:3.12-alpine sh -c \
--     'pip install -q pyyaml && python -c "import yaml;open(\".luatest/health.lua\",\"w\").write(yaml.safe_load(open(\"examples/argocd-health-check.yaml\"))[\"data\"][\"resource.customizations.health.platform.glueops.dev_Gate\"])"'
--   docker run --rm -v "$PWD/.luatest":/t -w /t nickblah/lua:5.4-alpine \
--     lua argocd-health-check_test.lua
--   rm -rf .luatest

local src = assert(io.open("health.lua")):read("a")
local chunk = assert(load(src, "health.lua"))

-- Argo CD sets a global `obj` and runs the script, so do the same.
local function run(o)
  obj = o
  return chunk()
end

local function cond(status, reason, message)
  return { { type = "Ready", status = status, reason = reason, message = message } }
end

local function gate(generation, observed, conditions)
  return {
    metadata = { generation = generation },
    status = { observedGeneration = observed, conditions = conditions },
  }
end

local cases = {
  { "ready, generations in step",
    gate(3, 3, cond("True", "Ready", "2 checks passed")), "Healthy" },

  { "not ready",
    gate(3, 3, cond("False", "DependenciesNotReady", "cache: ready addresses 0 < 1")), "Progressing" },

  -- Terminal is what makes Argo CD fail a gated sync promptly rather than waiting
  -- out --sync-timeout on a dependency that will never recover.
  { "terminal",
    gate(3, 3, cond("False", "Terminal", "migration: job Failed=True (BackoffLimitExceeded)")), "Degraded" },

  -- Unevaluable makes no claim about the dependency, so it must not read Degraded.
  { "unknown / unevaluable",
    gate(3, 3, cond("Unknown", "EvaluationFailed", "db: read failed")), "Progressing" },

  -- The guard. A True condition describing a superseded spec must not pass.
  { "generation skew with a stale True condition",
    gate(4, 3, cond("True", "Ready", "2 checks passed")), "Progressing" },

  { "no status at all",
    { metadata = { generation = 1 } }, "Progressing" },

  { "status with no conditions key",
    { metadata = { generation = 1 }, status = { observedGeneration = 1 } }, "Progressing" },

  { "empty conditions list",
    gate(1, 1, {}), "Progressing" },

  { "no Ready condition among others",
    gate(1, 1, { { type = "SomethingElse", status = "True", reason = "Nope", message = "x" } }), "Progressing" },

  -- A Gate that has never been reconciled has no observedGeneration.
  { "observedGeneration absent",
    { metadata = { generation = 1 }, status = { conditions = cond("True", "Ready", "x") } }, "Progressing" },

  -- A condition with no message must not break string concatenation.
  { "condition without a message",
    gate(1, 1, { { type = "Ready", status = "False", reason = "DependenciesNotReady" } }), "Progressing" },
}

local fails = 0
for _, c in ipairs(cases) do
  local name, input, want = c[1], c[2], c[3]
  local ok, hs = pcall(run, input)
  local got = ok and (hs.status or "(nil status)") or ("ERROR: " .. tostring(hs))
  local pass = got == want
  if not pass then fails = fails + 1 end
  print(string.format("%-5s %-46s got=%-12s want=%s",
    pass and "PASS" or "FAIL", name, got, want))
end

if fails > 0 then
  print("\n" .. fails .. " of " .. #cases .. " FAILED")
  os.exit(1)
end
print("\nall " .. #cases .. " cases pass")

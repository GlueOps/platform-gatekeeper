-- Tests for the Lua health check in argocd-health-check.yaml.
--
-- This script is load-bearing and fails silently when wrong: Argo CD ignores a
-- resource whose health check errors, so the wave hangs with the Application
-- showing Unknown and nothing alerting. The generation guard is the sharpest part —
-- without it an edited Gate is judged against the condition left from its previous
-- spec, so Argo CD sees Healthy and proceeds, bypassing the gate on the very sync
-- that tightened it.
--
-- Run it from the repo root (the script lives inside a ConfigMap value, so extract it
-- first). Same commands .github/workflows/ci.yaml runs, minus the digest pins:
--
--   mkdir -p .testtmp
--   cp examples/argocd-health-check_test.lua examples/argocd-health-check.yaml .testtmp/
--   docker run --rm -v "$PWD/.testtmp":/workdir mikefarah/yq:4.47.1 \
--     '.data["resource.customizations.health.platform.glueops.dev_Gate"]' \
--     argocd-health-check.yaml > .testtmp/health.lua
--   for v in 5.1 5.4; do \
--     docker run --rm -v "$PWD/.testtmp":/t -w /t nickblah/lua:$v-alpine \
--       lua argocd-health-check_test.lua; done
--   rm -rf .testtmp
--
-- Both dialects, not just 5.4: Argo CD embeds gopher-lua, a Lua 5.1 dialect, so a
-- 5.4-only run would accept syntax the real runtime rejects — which is why the two
-- lines below take their 5.1 spellings. The harness is still more permissive than
-- Argo CD's sandbox; AGENTS.md has the caveat.

local src = assert(io.open("health.lua")):read("*a") -- "a" is 5.2+
local chunk = assert((loadstring or load)(src, "health.lua")) -- loadstring is 5.1

-- Argo CD sets a global `obj` and runs the script, so do the same.
local function run(o)
  obj = o
  return chunk()
end

local function cond(status, reason, message, observedGeneration)
  return { {
    type = "Ready", status = status, reason = reason, message = message,
    observedGeneration = observedGeneration,
  } }
end

local function gate(generation, observed, conditions)
  return {
    metadata = { generation = generation },
    status = { observedGeneration = observed, conditions = conditions },
  }
end

-- A 4th field, where given, is the expected hs.message: it can be dropped without
-- changing any status, yet it is all the operator sees in the Argo CD UI.
local cases = {
  { "ready, generations in step",
    gate(3, 3, cond("True", "Ready", "2 checks passed")), "Healthy",
    "Ready: 2 checks passed" },

  { "not ready",
    gate(3, 3, cond("False", "DependenciesNotReady", "cache: ready addresses 0 < 1")), "Progressing",
    "DependenciesNotReady: cache: ready addresses 0 < 1" },

  -- Terminal is what makes Argo CD fail a gated sync promptly rather than waiting
  -- out --sync-timeout on a dependency that will never recover.
  { "terminal",
    gate(3, 3, cond("False", "Terminal", "migration: job Failed=True (BackoffLimitExceeded)")), "Degraded",
    "Terminal: migration: job Failed=True (BackoffLimitExceeded)" },

  -- Unevaluable makes no claim about the dependency, so it must not read Degraded.
  { "unknown / unevaluable",
    gate(3, 3, cond("Unknown", "EvaluationFailed", "db: read failed")), "Progressing",
    "EvaluationFailed: db: read failed" },

  -- The other three reasons that cannot clear without a human edit. Each needs a
  -- change outside the dependency -- the Gate's spec, the namespace's mode label, or
  -- gatekeeper's ClusterRole -- so waiting is pointless.
  { "policy violation",
    gate(3, 3, cond("False", "PolicyViolation", "dep: namespace kube-system not allowed")), "Degraded",
    "PolicyViolation: dep: namespace kube-system not allowed" },

  { "read forbidden",
    gate(3, 3, cond("False", "ReadForbidden", "cache: deployments read forbidden")), "Degraded",
    "ReadForbidden: cache: deployments read forbidden" },

  { "invalid spec",
    gate(3, 3, cond("False", "InvalidSpec", "dep: expected exactly 1 check type, got 0")), "Degraded",
    "InvalidSpec: dep: expected exactly 1 check type, got 0" },

  -- The guard against over-widening: the normal waiting state must never be Degraded,
  -- or every gate that is simply doing its job fails the sync.
  { "dependencies not ready stays Progressing",
    gate(3, 3, cond("False", "DependenciesNotReady", "db: 0/1 available")), "Progressing",
    "DependenciesNotReady: db: 0/1 available" },

  -- The guard. A True condition describing a superseded spec must not pass.
  { "generation skew with a stale True condition",
    gate(4, 3, cond("True", "Ready", "2 checks passed")), "Progressing" },

  -- The guard's other direction, and the normal remediation path: an author edits a
  -- Gate to fix a terminal dependency. Judged against the leftover condition the
  -- Gate reads Degraded, which fails the very sync carrying the fix.
  { "generation skew with a stale Terminal condition",
    gate(4, 3, cond("False", "Terminal", "migration: job Failed=True")), "Progressing" },

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

  -- The condition's own observedGeneration is what metav1 binds to the condition,
  -- so a stale one must not pass even when the status-level field looks current.
  -- Reading only the status-level field here returns Healthy: the bypass.
  { "stale condition-level observedGeneration, current status-level",
    gate(4, 4, cond("True", "Ready", "2 checks passed", 3)), "Progressing" },

  { "current condition-level observedGeneration",
    gate(4, 4, cond("True", "Ready", "2 checks passed", 4)), "Healthy" },

  -- Fail closed when currency cannot be established at all.
  { "metadata absent",
    { status = { observedGeneration = 1, conditions = cond("True", "Ready", "x") } }, "Progressing" },

  { "generation absent",
    { metadata = {}, status = { observedGeneration = 1, conditions = cond("True", "Ready", "x") } }, "Progressing" },
}

local fails = 0
for _, c in ipairs(cases) do
  local name, input, want, wantMsg = c[1], c[2], c[3], c[4]
  local ok, hs = pcall(run, input)
  local got = ok and (hs.status or "(nil status)") or ("ERROR: " .. tostring(hs))
  local pass = got == want
  local note = ""
  if pass and wantMsg then
    local msg = ok and hs.message or nil
    if msg ~= wantMsg then
      pass = false
      note = string.format("  message=%q want %q", tostring(msg), wantMsg)
    end
  end
  if not pass then fails = fails + 1 end
  print(string.format("%-5s %-46s got=%-12s want=%s%s",
    pass and "PASS" or "FAIL", name, got, want, note))
end

if fails > 0 then
  print("\n" .. fails .. " of " .. #cases .. " FAILED")
  os.exit(1)
end
print("\nall " .. #cases .. " cases pass")

# Scope

What Bifrost is for, what shape it keeps, and the test a new capability has to
pass before it ships. If a proposal and this document disagree, this document
wins until it is amended on purpose.

## The position

Bifrost is a **homelab AI gateway**: one operator, a handful of agents, a
handful of backends, one machine that may be a Pi.

It is an alternative to the enterprise AI gateways — the ones built for
organisations with teams, budgets to allocate, identity providers to integrate,
and procurement to satisfy. Those solve a real problem. It is not ours. Their
feature set assumes a platform team and a database; ours assumes one person who
wants to know what their agents cost and to keep working when a backend falls
over.

Same category, different customer. We are the replacement for the enterprise
gateway in the one place it does not fit: a house.

> Not affiliated with Maxim AI's Bifrost, or any other project of the same name.
> Different codebase, different scope, MIT.

## Who the callers actually are

Named, because a feature that serves nobody is scope, not progress:

- **Guppy** — chat through DeepSeek, speech through ElevenLabs.
- **Jeeves** — chat through DeepSeek, on its own key and its own budget.
- **Cassandra** — chat through DeepSeek, on its own key.
- **Subwave** — chat and speech, on its own key.
- **Claude routes** — present for the occasions when a paid escalation is
  worth it. They serve, they are priced, and since 2026-11 they carry prompt-cache
  breakpoints, which is what makes a long escalation affordable. They are not a
  default: the cost per call is an order of magnitude above DeepSeek's.

Four backends exist in practice: DeepSeek, Anthropic, an Ollama-compatible
local endpoint, and ElevenLabs. That list is the deployment today, not a fence
around tomorrow.

## The objective, restated

The reason this exists is not protocol translation — every gateway does that.
It is that **model access should be a decision made at one seam, and a fact
recorded at that seam**: which backend, under which policy, on whose budget, at
what cost. The gateway is where that decision is made and where the receipt is
written.

Everything else in the codebase is in service of that sentence, or it is
furniture.

## We may grow sideways

Breadth is not the enemy. The enterprise gateways are running a public,
well-funded experiment in what an AI gateway can do, and the sensible move is to
read their results rather than pretend we thought of everything.

**Borrowing is expected.** Specifically from Maxim's Bifrost: failover and
load-balancing behaviour, budget and limit models, caching strategies, request
attribution and metadata, model catalogues, and their operational tooling. When
one of those fits our deployment, it is fair game.

**Borrowing code is allowed, with the licence respected.** Their gateway is
Apache-2.0. Ideas can be reimplemented freely; code that is copied keeps its
licence headers and NOTICE, is marked in the file, and is named in the commit
message. What we do not import is their *shape*: module dependencies, plugin
runtime, database. Port the idea into the standard library, or leave it.

The line to hold is not "fewer features". It is this:

> **The shape is fixed even when the feature list is not: one static binary,
> standard library, two plain files of state.**

A borrow that fits that shape is growth. A borrow that brings a database, a
sidecar, a plugin runtime or a multi-service topology is a different product
wearing our name.

## The test

A new capability ships when it clears all three:

1. **It answers something we have hit** — a named caller in the list above, the
   operator reading the dashboard, or a failure we have actually watched happen.
   Borrowing a feature from elsewhere is fine; borrowing it *because it exists*
   is not.
2. **It needs no external state.** One static binary, standard library, two
   plain files on a volume. A feature that requires a database, a queue, a
   sidecar or a plugin runtime is a different product.
3. **It survives the bad day.** The LAN is up, the WAN is flaky, one backend is
   down, and the machine is a Pi. If the feature only works in a well-funded
   network, it is the wrong feature.

Failing the test does not mean "later". It means **not planned**, and it belongs
in the list below rather than on a backlog nobody will drain.

## What we won't become

These are structural, not aesthetic. Each one presupposes an organisation, and
adopting it would break the shape above before it improved anything:

- **SSO, OIDC, teams, roles, business units.** There is one operator and there
  are four keys. Identity providers are how a company solves a problem a house
  does not have.
- **Multi-node clustering.** One binary, one process, one volume. If it needs a
  consensus layer it needs a platform team.
- **Billing, invoicing, chargeback.** The ledger records spend. Turning spend
  into money owed is an accounting product.
- **An admin console as a product.** The dashboard reads. It does not operate a
  fleet.

Everything else is a judgement call, made per feature, against the test above.
That includes things previously written off here: an MCP-style tool seam, richer
caching, and more backends are all open questions, not refusals.

## What narrowing means here

Nothing that exists gets removed. The cache, the Ollama dialect, the embeddings
path and the provider adapters stay where they are — they work, they are tested,
and they cost nothing at runtime when unused.

Narrowing means the *shape* does not drift, and effort goes in a fixed order:

1. **The seam** — routing, policy, metering.
2. **The operator's view** — ledger, dashboard, metrics.
3. **Compatibility and breadth** — new dialects, backends and borrowed
   features, when a caller or a real failure asks for them.

The measure of progress is not the feature list getting shorter, and not the
binary getting bigger. It is the sentence above getting truer: one seam, every
backend, every request accounted to the agent that made it, on a machine that
fits under a desk.

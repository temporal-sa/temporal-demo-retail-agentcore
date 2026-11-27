# Runbook: Crew orders agent on AgentCore

## What it shows

A member asks the café app's agent, Ziggy, to repeat last Friday's crew order with changes. Ziggy
rebuilds and prices it, its sandbox ends while the member decides, and a new sandbox charges the
card exactly once. Your Strands agent, made durable with Temporal, running on AgentCore.

It makes three problems visible ([docs/story.md](docs/story.md)):

1. AgentCore ends an idle session, so the wait for the member outlives the sandbox that built the
   cart.
2. The agent's work can't be redone the same way: run it again and the model may build a different
   cart from the one the member approved.
3. A retried or resumed checkout can charge the card twice.

## Before you start

- **Where:** the catalog card "Crew Orders Agent on AgentCore" on tmprl-demo.cloud opens
  `https://retail-agentcore.<catalog domain>`. You need a Temporal sign-in for the catalog.
- **Shared deployment, separate orders.** There is no crashable copy. Every presenter shares one
  agent runtime, and each browser sees only its own orders. Another presenter's order can wake the
  Worker while you talk; the Compute card shows that honestly.
- **Before the audience arrives:** open the demo and check the Compute card says "Idle · 0
  running". If a sandbox is running, wait a minute for it to end, so your first order shows a cold
  start.

## Start it

Open the demo. A healthy start shows the phone with Zora's request ready to send, Ziggy saying "Send
the order from the phone to start", the Compute card idle and the Store ledger at zero. The "How
Temporal runs Ziggy on AgentCore" panel is open; collapse it with **Hide** if the screen is small.

## The walkthrough

### 1. Ask Ziggy for the crew order

Read the request on the phone aloud, then tap send.

The audience sees "Now: starting a sandbox", then Sandbox 1 running, then Ziggy's checks arrive on
the phone and as REST calls in the Tool calls card: order history, menu, stock, rewards, a quote.

Say: "Nothing was running a second ago. Starting the order gave Temporal work, and Temporal Cloud
invoked the agent on AgentCore. Ziggy decides which store calls to make. Each one, and each model
call, is recorded in Temporal."

### 2. The cart (why this needs an agent)

The cart appears with Pay. Point at the changes: oat milk is out at Titan Ring, so the latte is
almond; Mo's mocha is almond with no cream; Mo's egg wrap is now a Ceres veggie wrap; Crew Pack saves
8.51 cr, and Ziggy says which other rewards it checked.

Say: "No form takes that request. The model read the history, the menu and the stock and made those
calls itself. Ask it again and it might do it differently. That matters in a minute."

Don't tap Pay yet.

### 3. The sandbox ends while the member decides (problem 1)

Wait about a minute. The Compute card changes to **No sandbox running** and Ziggy greys out.

Say: "The Worker ran out of work and drained, and AgentCore ended the idle session. Nothing is
running and nothing is billing. The cart on the phone, every model answer and every store reply
are in Temporal's Event History, not in a process."

Proof: the Compute card, read from Temporal's Worker heartbeats. The phone still shows the cart,
read with Describe, not from the agent.

### 4. Pay: a new sandbox finishes the same order (problems 2 and 3)

Tap **Pay 48.24 cr**.

"Now: starting a sandbox", Sandbox 2 starts, the Tool calls card adds `POST /payments`, `POST
/orders` and `POST /members/{member}/stardust` on Sandbox 2, and Ziggy confirms the order number and
pickup time.

Say: "That tap was recorded the moment I made it, with nothing running. Recording it started compute
again. The new sandbox replayed the history, so the agent had the same cart without asking the model
to redo its work. Only the payment, the order and one reply ran."

Proof: Model calls went up by one, and "repeated after resume" is 0. The Store ledger shows one card
charge for 48.24 cr and "charged twice: 0". Open **Open in Temporal UI** for the full history.

### 5. Optional: ask for a change before paying

On a new order, instead of paying, type "add two more donuts" and send. Locally the stub model says
it can only build the scripted order; with Bedrock, Ziggy reprices the cart and Pay uses the new
quote. A Pay for the old cart is ignored.

## Show it in code

Line links point at paths on `main` until the first commit; replace them with commit permalinks then
(guide 18).

### Run a Strands agent as a durable Workflow

**When:** "What did it take to make the agent durable?"

**Where:** [`agent/crew/workflows.py:99-111`](agent/crew/workflows.py#L99-L111) (`code -g agent/crew/workflows.py:99`)

```python
self._agent = TemporalAgent(
    model=MODEL_NAME,
    start_to_close_timeout=timedelta(seconds=60),
    retry_policy=RetryPolicy(maximum_attempts=5),
    system_prompt=SYSTEM_PROMPT,
    tools=[_tool_activity(t) for t in activities.TOOLS],
    hooks=[Progress(self)],
    callback_handler=None,
)
```

**What to understand:** `TemporalAgent` is a Strands `Agent` whose model calls run as the plugin's
`invoke_model` Activity, and `activity_as_tool` (in `_tool_activity`) makes each tool an Activity.
The tools are ordinary async functions with docstrings that call the store's REST API; Strands
builds their schemas. The model name maps to a factory registered on the Worker, so the Bedrock
client lives on the Worker, never in the Workflow. Docs:
[temporalio-strands-agents](https://pypi.org/project/temporalio-strands-agents/).

### Wait for the member with nothing running

**When:** beat 3, "what is running while it waits?"

**Where:** [`agent/crew/workflows.py:123-136`](agent/crew/workflows.py#L123-L136) and the Signal at [`160-171`](agent/crew/workflows.py#L160-L171) (`code -g agent/crew/workflows.py:123`)

```python
while True:
    self._say("ziggy", str(reply).strip())
    self._set_phase("confirm" if self._quote else "chat")
    try:
        await workflow.wait_condition(lambda: bool(self._inbox), timeout=PAYMENT_TIMEOUT)
    except asyncio.TimeoutError:
        self._set_phase("expired")
        return OrderResult(phase="expired")
    msg = self._inbox.pop(0)
    if isinstance(msg, Payment):
        break
    # ...
```

**What to understand:** `wait_condition` is a durable wait: the Worker can drain and AgentCore can
end the session, and the `pay` Signal brings a new Worker back to this line. A Signal, not an
Update, because the server records it at once with no Worker running, and recording it is what
makes Temporal Cloud invoke one. The phone reads the cart from the memo (`_set_phase`), never with a
Query.

### Charge exactly once

**When:** beat 4, "what stops a double charge?"

**Where:** [`agent/crew/workflows.py:138-150`](agent/crew/workflows.py#L138-L150) and the store's [`store/internal/store/ledger.go:58-90`](store/internal/store/ledger.go#L58-L90) (`code -g agent/crew/workflows.py:138`)

```python
key = workflow.info().workflow_id
checkout = {"start_to_close_timeout": timedelta(seconds=10), "retry_policy": RetryPolicy(maximum_attempts=10)}
payment = await workflow.execute_activity(activities.charge_payment, args=[key, quote["quoteId"]], **checkout)
order = await workflow.execute_activity(activities.place_order, args=[key, quote["quoteId"], payment["paymentId"]], **checkout)
award = await workflow.execute_activity(activities.award_stardust, args=[key, req.member_id, quote["earns"], order["orderNumber"]], **checkout)
```

```go
// The write happens once per key, whoever asks and however often.
ConditionExpression: aws.String("attribute_not_exists(pk)"),
```

**What to understand:** checkout is in the Workflow, not a tool, so the model never decides to
charge a card. Once an Activity completes, the history keeps it from running again on any Worker.
The one gap left is a sandbox lost after the store took the payment and before the result reached
Temporal; the retry then sends the same Idempotency-Key (the Workflow ID), and the store's
conditional write answers with the first result.

### Show state without a running Worker

**When:** "How does the screen know what's happening if nothing is running?"

**Where:** [`api/internal/crew/server.go:67-121`](api/internal/crew/server.go#L67-L121) (`code -g api/internal/crew/server.go:67`)

**What to understand:** one read per poll, its calls run together: Describe (memo and pending
tasks), the Event History (the proof rows), ListWorkers heartbeats (the Compute card) and the
store's ledger. None needs a Worker, none is a billable Action, and none wakes the agent. A Query
would do all three wrong. A heartbeat older than 25 s counts as a gone Worker.

## Reset and tear down

Nothing to reset between presenters: each order is its own Workflow, and each browser sees only its
own. Start another with **New order**.

Nothing keeps running. A finished order uses no compute, and an unpaid one closes itself after 30
minutes (its timer is the only thing it costs while it waits). AgentCore ends each idle session
after 60 seconds. Ledger rows expire after 7 days.

One order costs 19 Actions on Temporal Cloud: the start, 16 Activities (7 model calls, 5 tool calls,
3 checkout steps and the plugin's one model-config read), the payment timer and the Signal, counted
from a real run's Event History with the stub model. Bedrock may make a different number of model
calls. The memo updates and the screen's reads cost none.

## When something goes wrong

| Symptom | Likely cause | Fix |
|---|---|---|
| "Starting a sandbox" stays up for more than 30 s | Temporal Cloud can't invoke the runtime, or the Worker fails at start | In the Temporal UI, open the Worker Deployment and check the version is current; check the runtime's log group `/aws/bedrock-agentcore/runtimes/<runtime id>-<endpoint>` |
| A model step fails with `AccessDeniedException` | Bedrock model access isn't enabled in the AWS account | An AWS admin enables the model once per account (docs/catalog.md, 8) |
| Tool calls fail and Ziggy says it couldn't read the store | The agent can't reach the store's public route, or the token differs | Open `https://<demo host>/store/v1/cafes`: 401 means the route works. Check `STORE_API_URL` in the DemoProject names this host |
| Pay does nothing | The cart changed after the phone showed it, or the order closed after 30 minutes | Start a new order |
| The Tool calls card says "stub model, offline" on the hosted demo | `AGENT_MODEL=stub` in the component's env | Set `AGENT_MODEL: bedrock` in the DemoProject |

## Run it locally

Prerequisites: Docker, Docker Compose and `make`. Variables are listed in `.env.example`.

```sh
make demo        # build and start: dev server, DynamoDB Local, store, agent, local-invoker, api, web
make test        # Go store and API tests and the Python Workflow tests, in containers
make e2e         # the whole story through the published ports, against `make demo`
make kill-agent  # replace the agent's process, like a new AgentCore session
make invoke      # POST /invocations by hand, as Temporal Cloud would
make arm64       # build the agent image for linux/arm64, the only architecture AgentCore runs
make reset       # back to the starting state
```

- Demo: http://localhost:8080. Temporal UI: http://localhost:8233. The agent's AgentCore contract:
  http://localhost:8081/ping and `/invocations`. The store: http://localhost:8082/store/v1 with
  `Authorization: Bearer local-store-token-not-a-secret`.
- **Who invokes the agent locally.** `local-invoker` stands in for Temporal Cloud: it makes the
  Worker Deployment Version current, then POSTs `/invocations` whenever the version has backlog and
  no live Worker. On the platform, Temporal Cloud does this and `local-invoker` does not exist.
- **The model.** Locally the agent uses a scripted stub (`AGENT_MODEL=stub`), which follows the
  same tool calls a model would and says so on screen. To use Bedrock, put `AGENT_MODEL=bedrock`,
  `BEDROCK_MODEL_ID`, `AWS_REGION` and AWS credentials in `.env`, then `make demo`.
- **Timing.** Compose sets `AGENTCORE_DEBOUNCE_SECONDS=20`, so the Worker drains 20 s after the
  cart. Restarting the agent container (`make kill-agent`) shows a recycled runtime.
- **Troubleshooting.** `docker compose logs agent local-invoker store` shows each invocation, each
  Worker start and drain, and each store request. The agent exits at start with "cannot start: ..."
  when a required variable is missing.

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

The support chat after pickup adds two more:

4. An agent that can move money needs limits it can't talk its way past, and some calls need a
   person. Here that's a deterministic flow inside the agent's tool: the agent doesn't decide to
   escalate and doesn't know it happened.
5. Support waits are long and uneven (the member, a support agent, a manager), and the agent is in
   the middle of a tool call through them.

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
the order from the phone to start", the Kitchen card with no order, the Compute card idle and the
Store ledger at zero.

## The walkthrough

### 1. Ask Ziggy for the crew order

Read the request on the phone aloud, then tap send.

The audience sees Ziggy "Waking up in a new sandbox", then Sandbox 1 running, then Ziggy's checks arrive on
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

Sandbox 2 starts, the Tool calls card adds `POST /payments`, `POST /orders` and `POST
/members/{member}/stardust` on Sandbox 2, and Ziggy confirms the order number and pickup time and
says "I'll let you know when your order is ready for pickup!"

Say: "That tap was recorded the moment I made it, with nothing running. Recording it started compute
again. The new sandbox replayed the history, so the agent had the same cart without asking the model
to redo its work. Only the payment, the order and one reply ran."

Proof: Model calls went up by one, and "repeated after resume" is 0. The Store ledger shows one card
charge for 48.24 cr and "charged twice: 0". Open **Open in Temporal UI** for the full history.

### 5. The kitchen marks it ready: another new sandbox

The Kitchen card shows the order as **Preparing**. Wait about a minute until the Compute card shows
**No sandbox running** again, then click **Mark order ready**.

Sandbox 3 starts, and Ziggy tells the member on the phone that the order is ready for pickup.

Say: "Ziggy promised to message the member when the order was ready, and then nothing ran while the
kitchen worked. The kitchen's click was another Signal. It started a new sandbox, which picked up
right after the promise and kept it."

Proof: the Event History shows `Signal received · kitchen_ready`, and Model calls went up by one
more. The Store ledger still shows one charge.

### 6. Support: a missing wrap, put right on one tap

The order's chat says "Chat closed". Tap **Need help with your order? Start a new chat**. The phone
sends "I need some help with my order OG-…" and Ziggy asks what went wrong. Type "My order was
missing the wrap" and send.

Ziggy asks how to fix it, with **Refund it**, **Make a fresh one at no charge** and **Something
else** as buttons. Wait about a minute: the sandbox ends while the buttons wait. Tap **Refund it**.

A new sandbox starts, the Tool calls card shows `issue_refund` and then `POST /refunds`, the Store
ledger's Refunded tile shows the amount, and Ziggy closes the chat.

Say: "The buttons are a tool call, offer_choices, that waits for the tap. The sandbox ended in the
middle of it, and the tap brought it back into the same call. The refund was within the café's
limits, so the tool just did it."

### 7. Support: a full refund waits for a person, and the agent never knows (problems 4 and 5)

Start another chat: "My entire order was terrible". Ziggy asks how to resolve it. Tap **Full
refund**.

Ziggy says "I've escalated this to a support agent, who will review it now." The Support desk card
shows the request with **Approve as support** and **Deny**, and a countdown; the step list shows
"Human review · inside the tool"; the Tool calls card shows `issue_refund` waiting. Wait a minute
for **No sandbox running**.

Say: "Ziggy didn't decide to escalate, and doesn't know it did. It called issue_refund. That tool is
plain code in the Workflow: it checked the café's policy, saw a 48 cr refund is over the limit, and
is now waiting for a person. Ziggy's tool call just hasn't come back yet. That's a decision we
don't want a model making, and here it can't: no prompt can talk its way past an if statement. And
the wait costs nothing: there's no sandbox."

Then either click **Approve as support**, or don't, and let the timers show:

- After 90 seconds the Event History shows "Timer fired", Ziggy says the request went to a manager,
  and the card says **Manager review**.
- After 90 more it shows "Timer fired" again and the tool approves the refund itself.

Either way a new sandbox starts, the tool carries on from where it waited, calls `POST /refunds`
once, and returns "Done" to Ziggy, which tells the member.

Proof: Refunded 48.24 on the Store ledger, once; "repeated after resume" is 0.

### 8. Optional: ask for a change before paying

On a new order, instead of paying, type "add two more donuts" and send. Locally the stub model says
it can only build the scripted order; with Bedrock, Ziggy reprices the cart and Pay uses the new
quote. A Pay for the old cart is ignored.

## Show it in code

Line links point at paths on `main` until the first commit; replace them with commit permalinks then
(guide 18).

### Run a Strands agent as a durable Workflow

**When:** "What did it take to make the agent durable?"

**Where:** [`agent/crew/workflows.py:114-126`](agent/crew/workflows.py#L114-L126) (`code -g agent/crew/workflows.py:114`)

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

**When:** beats 3 and 5, "what is running while it waits?"

**Where:** [`agent/crew/workflows.py:138-151`](agent/crew/workflows.py#L138-L151) and the Signal at [`190-201`](agent/crew/workflows.py#L190-L201); the kitchen's wait at [`174-181`](agent/crew/workflows.py#L174-L181) and its Signal at [`211-217`](agent/crew/workflows.py#L211-L217) (`code -g agent/crew/workflows.py:138`)

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
Query. The wait for the kitchen is the same pattern: `wait_condition` on a flag that the
`kitchen_ready` Signal sets.

### Charge exactly once

**When:** beat 4, "what stops a double charge?"

**Where:** [`agent/crew/workflows.py:153-165`](agent/crew/workflows.py#L153-L165) and the store's [`store/internal/store/ledger.go:58-90`](store/internal/store/ledger.go#L58-L90) (`code -g agent/crew/workflows.py:153`)

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

### Escalate inside the tool, where the agent can't see it

**When:** beat 7, "who decided this needed a human?"

**Where:** the tool at [`agent/crew/support.py:256-264`](agent/crew/support.py#L256-L264), the
policy at [`103-135`](agent/crew/support.py#L103-L135), the escalation at
[`311-346`](agent/crew/support.py#L311-L346) (`code -g agent/crew/support.py:311`)

```python
async def _act(self, kind, reason, *, amount=0.0, item_ids=None, points=0) -> str:
    status, why, value = check(kind, order, amount=amount, item_ids=item_ids, points=points)
    if status == "refused":
        return f"Not possible: {why}."
    if status == "approval":
        approved, decided_by = await self._review(TOOL_NAMES[kind], what, why, reason)
        if not approved:
            return "The café declined this request."
    result = await self._carry_out(kind, reason, amount=value, item_ids=item_ids or [], points=points)
    return f"Done: {result['summary']}."

# _review: a support agent, then a manager, then the timer
for tier, seconds, line in tiers:
    self._say("ziggy", line)  # fixed text, not the model's
    try:
        await workflow.wait_condition(lambda: self._decision is not None, timeout=timedelta(seconds=seconds))
    except asyncio.TimeoutError:
        continue
    return self._decision.approve, tier
return True, "timer"
```

**What to understand:** this is the point of the second act. The escalation is a deterministic flow
in code, not something the agent decides, and the agent isn't aware of it. Nothing in the system
prompt or the tool descriptions mentions approval; the model sees a tool that returns "Done",
"declined" or "Not possible". The plugin lets a Strands tool be ordinary Workflow code, so the tool
itself can check a policy, wait on a Signal (`review`) or a timer with `wait_condition`, and call an
Activity, all durably. While it waits the Worker drains and AgentCore ends the session; the next
Signal or timer starts a new Worker, which replays the agent back into the same tool call. The
"I've escalated this" line is posted by the Workflow, in Ziggy's voice. Use this shape for any
action where the rule must hold whatever the model says: refunds, credits, account changes.

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

A support chat closes itself once resolved, or 3 minutes after the member last said anything.

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
| Approve does nothing, or the chat stays on "Ziggy is working" | The review timer already moved the request on, or a new sandbox is still starting | Watch the Support desk card's tier and countdown; a decision applies to whoever holds the request now |
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

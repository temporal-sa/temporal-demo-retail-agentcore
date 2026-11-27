"""The crew order Workflow: a Strands agent made durable by Temporal.

Ziggy, the agent, turns a member's message ("same as last Friday, but Mo's
vegan now") into a priced cart by calling the store's API, as tools. The
member then pays in the app, which can take minutes. Nothing runs while they
decide: the Worker drains and AgentCore ends its session. The payment Signal
starts a new Worker, which replays this Workflow's Event History. Every model
answer and tool result comes back from the history, so the agent carries on
with the cart the member saw, and the checkout runs once.

The checkout itself is not up to the model. Once the member pays, this
Workflow charges, places the order and awards Stardust in that order, each
with the Workflow ID as the store's Idempotency-Key.

The UI never sends a Query: Queries need a running Worker, and on AgentCore
there usually isn't one. It reads the memo, which this Workflow keeps
current, with DescribeWorkflowExecution.
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy

with workflow.unsafe.imports_passed_through():
    from strands.hooks import HookProvider, HookRegistry
    from strands.hooks.events import AfterToolCallEvent
    from temporalio.strands_agents import TemporalAgent
    from temporalio.strands_agents.workflow import activity_as_tool

    from . import activities
    from .models import MODEL_NAME

# How long a cart waits for the member. A real app would remind them first.
PAYMENT_TIMEOUT = timedelta(minutes=30)

SYSTEM_PROMPT = """You are Ziggy, the ordering assistant in a café chain's app. A member asks you
to put together an order for pickup. Use your tools; never guess items, prices, stock or rewards.

1. Before building the cart, read the member's recent orders, the pickup café's menu and stock,
   and their rewards.
2. Apply what the member asked for. For a dietary need, check ingredients: use options (another
   milk, no cream) or a similar item so each affected line meets it, and keep the line's note.
3. Replace anything the café is out of with the closest option.
4. Price the cart with quote_cart, reward_id "best" unless the member named a reward. If it is
   refused, fix every problem it lists and price it again. Your last quote_cart call must be the
   cart you recommend: the member sees and pays for that one.
5. Reply to the member in at most three short sentences: what you changed from their usual order,
   the reward applied, and the total. The app shows the cart and a Pay button, so don't list every
   item or ask them to confirm.

You can't take payment or place the order. The member pays in the app."""


@dataclass
class OrderRequest:
    member_id: str
    cafe_id: str
    text: str


@dataclass
class Payment:
    quote_id: str


@dataclass
class MemberMessage:
    text: str


@dataclass
class OrderResult:
    phase: str
    order_number: str = ""


def _tool_activity(fn: Any) -> Any:
    # The store answers in well under a second. Ten seconds covers a slow
    # call; five attempts cover a store restart.
    return activity_as_tool(fn, start_to_close_timeout=timedelta(seconds=10), retry_policy=RetryPolicy(maximum_attempts=5))


@workflow.defn
class CrewOrderWorkflow:
    @workflow.init
    def __init__(self, req: OrderRequest) -> None:
        self._inbox: list[Payment | MemberMessage] = []
        self._quote: dict[str, Any] | None = None
        self._memo: dict[str, Any] = {"phase": "planning", "messages": [], "work": [], "quote": None, "order": None}
        # Model calls and tool calls are Activities: the plugin records each
        # one in the history, so a replay reuses its result.
        self._agent = TemporalAgent(
            model=MODEL_NAME,
            # Bedrock answers in seconds; a minute covers a slow turn. The
            # plugin heartbeats every model call, so a lost Worker is noticed
            # in 10 seconds, not at this timeout.
            start_to_close_timeout=timedelta(seconds=60),
            retry_policy=RetryPolicy(maximum_attempts=5),
            system_prompt=SYSTEM_PROMPT,
            tools=[_tool_activity(t) for t in activities.TOOLS],
            hooks=[Progress(self)],
            # Strands prints the stream to stdout by default, on every replay too.
            callback_handler=None,
        )

    @workflow.run
    async def run(self, req: OrderRequest) -> OrderResult:
        self._say("member", req.text)
        prompt = f"Member member_id={req.member_id} is ordering for pickup at cafe_id={req.cafe_id}. Their message: {req.text}"
        reply = await self._agent.invoke_async(prompt)
        self._publish(model=self._agent.model.get_config().get("model_id"))

        # Wait for the member: Pay, or a message asking for a change. This is
        # a durable wait. The Worker can drain and go away, and the Signal
        # brings a new one back to this line.
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
            self._say("member", msg.text)
            self._set_phase("planning")
            reply = await self._agent.invoke_async(msg.text)

        quote = self._quote
        assert quote is not None  # pay() accepts only the current quote
        self._set_phase("placing")
        # The Workflow ID names this order everywhere: a retried Activity, or
        # a new Worker after the old one is gone, sends the same key, and the
        # store answers with what it already did.
        key = workflow.info().workflow_id
        checkout = {"start_to_close_timeout": timedelta(seconds=10), "retry_policy": RetryPolicy(maximum_attempts=10)}
        payment = await workflow.execute_activity(activities.charge_payment, args=[key, quote["quoteId"]], **checkout)
        order = await workflow.execute_activity(activities.place_order, args=[key, quote["quoteId"], payment["paymentId"]], **checkout)
        award = await workflow.execute_activity(activities.award_stardust, args=[key, req.member_id, quote["earns"], order["orderNumber"]], **checkout)
        ready = order["readyAt"][11:16]
        self._publish(order={**order, "ready": ready, "points": award["points"], "balance": award["balance"], "amount": payment["amount"]})

        reply = await self._agent.invoke_async(
            f"The member paid {payment['amount']} cr. Order {order['orderNumber']} will be ready at {ready} at {order['cafeName']}. "
            f"{award['points']} Stardust were added. Tell them in one sentence."
        )
        self._say("ziggy", str(reply).strip())
        self._set_phase("placed")
        return OrderResult(phase="placed", order_number=order["orderNumber"])

    @workflow.signal
    def pay(self, payment: Payment) -> None:
        """The member tapped Pay on a cart.

        A Signal, not an Update: the server records it at once, with no
        Worker running, and that is what wakes one. A payment for a cart that
        is not the current one is ignored.
        """
        if self._memo["phase"] != "confirm" or not self._quote or payment.quote_id != self._quote["quoteId"]:
            workflow.logger.warning("payment ignored; it is not for the cart on screen")
            return
        self._inbox.append(payment)

    @workflow.signal
    def message(self, msg: MemberMessage) -> None:
        """The member asked for a change instead of paying."""
        if self._memo["phase"] not in ("confirm", "chat"):
            workflow.logger.warning("message ignored; Ziggy is not waiting for one")
            return
        self._inbox.append(MemberMessage(text=msg.text[:500]))

    def _say(self, who: str, text: str) -> None:
        if text:
            self._publish(messages=self._memo["messages"] + [{"from": who, "text": text, "at": workflow.now().isoformat()}])

    def _set_phase(self, phase: str) -> None:
        self._publish(phase=phase)

    def _publish(self, **fields: Any) -> None:
        # The memo is what the UI reads. Upserting it costs nothing extra: it
        # rides on the Workflow Task the change happens in.
        self._memo.update(fields)
        workflow.upsert_memo(fields)


class Progress(HookProvider):
    """Copies each tool call into the memo, and keeps the latest priced cart."""

    def __init__(self, wf: CrewOrderWorkflow) -> None:
        self._wf = wf

    def register_hooks(self, registry: HookRegistry, **kwargs: Any) -> None:
        registry.add_callback(AfterToolCallEvent, self._tool_call)

    def _tool_call(self, event: AfterToolCallEvent) -> None:
        result = event.result
        text = "".join(c.get("text", "") for c in result.get("content", []))
        ok = result.get("status") == "success"
        body: dict[str, Any] = {}
        if ok:
            try:
                body = json.loads(text)
            except ValueError:
                pass
        name = event.tool_use["name"]
        if ok and name == activities.quote_cart.__name__:
            self._wf._quote = body
            self._wf._publish(quote=body)
        step = {"tool": name, "status": "completed" if ok else "failed", "text": body.get("summary", text[:300]), "at": workflow.now().isoformat()}
        self._wf._publish(work=self._wf._memo["work"] + [step])

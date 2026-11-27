"""The support chat Workflow: Ziggy as the café's support agent, after pickup.

The member taps "Need help with your order?" and the API starts this
Workflow with the order's details. Before Ziggy says anything, the Workflow
looks up the order's support record in the store: what was paid and what
support already did for it, from any chat. The member reports what went
wrong by picking items ("missing", "wrong") or the whole order or the
service, or by typing. Ziggy's tools run here, in the Workflow:

- offer_choices shows the member buttons and waits, durably, for a tap.
- issue_refund, place_comp_order and award_stardust_points do what they say.
  Each reads the order's record again and checks the café's policy in plain
  code first, so nothing is refunded or remade twice. Within its limits the
  store call runs at once. Over them, the tool escalates on its own: it tells
  the member, then waits for a support agent's decision on the page, then a
  manager's, then approves when the last timer fires. The agent never knows:
  it called a tool and got a result, minutes later.

Every wait (for the member, the support agent, the manager) is durable. The
Worker drains and AgentCore ends the session, and the next Signal or timer
starts a new one, which replays the history back into the middle of the
same tool call. The store calls are Activities keyed by this Workflow's ID,
so each refund, comp or award happens once.
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass, field
from datetime import timedelta
from typing import Any

from temporalio import workflow
from temporalio.common import RetryPolicy
from temporalio.exceptions import ActivityError, ApplicationError

with workflow.unsafe.imports_passed_through():
    from strands import tool
    from temporalio.strands_agents import TemporalAgent

    from . import activities
    from .models import MODEL_NAME
    from .workflows import MemberMessage, Progress

# What a tool may do with no one's approval, and the most anyone can approve.
REFUND_NOW_LIMIT = 15.00  # and never more than the lines it covers
COMP_NOW_LIMIT, COMP_MAX = 15.00, 60.00
STARDUST_NOW_LIMIT, STARDUST_MAX = 500, 5000

TOOL_NAMES = {"refund": "issue_refund", "comp": "place_comp_order", "stardust": "award_stardust_points"}

OTHER = "other"
OTHER_LABEL = "Something else"


@dataclass
class OrderLine:
    item_id: str
    name: str
    qty: int
    unit_price: float
    line_price: float
    options: list[str] = field(default_factory=list)
    note: str = ""


@dataclass
class OrderContext:
    """The order the member needs help with, from the order Workflow's memo."""

    order_key: str  # the order Workflow's ID, its payment's Idempotency-Key
    order_number: str
    cafe_id: str
    cafe_name: str
    member_id: str
    member_name: str
    payment_id: str
    amount_paid: float
    lines: list[OrderLine]


@dataclass
class SupportRequest:
    order: OrderContext
    greeting: str
    # What the member's first message says when they open the chat from
    # the app, before Ziggy's greeting.
    opener: str = ""
    # Short enough to show on the hosted copy, and each longer than
    # AgentCore's 60 s idle timeout, so every wait ends the sandbox.
    idle_timeout_seconds: int = 180
    review_timeout_seconds: int = 90
    manager_timeout_seconds: int = 90


@dataclass
class ReportedItem:
    item_id: str
    problem: str  # missing or wrong


@dataclass
class Report:
    """What the member picked on the "What went wrong?" card."""

    scope: str  # items, order or service
    items: list[ReportedItem] = field(default_factory=list)


@dataclass
class _Said:
    """A member's turn: what the chat shows, and what the agent reads."""

    display: str
    prompt: str


PROBLEMS = {"missing": "Missing", "wrong": "Wrong item"}


@dataclass
class Choice:
    option_id: str


@dataclass
class Review:
    approve: bool


@dataclass
class SupportResult:
    outcome: str  # resolved or expired


def settled(record: dict[str, Any]) -> dict[str, str]:
    """Items support already put right for this order, from any chat: item_id -> "refunded" or "remade"."""
    done: dict[str, str] = {}
    for r in record.get("remediations", []):
        for item in r.get("items") or []:
            if r["kind"] == "refund":
                done[item] = "refunded"
            elif r["kind"] == "comp":
                done.setdefault(item, "remade")
    return done


def check(kind: str, order: OrderContext, record: dict[str, Any], amount: float = 0.0, item_ids: list[str] | None = None, points: int = 0) -> tuple[str, str, float]:
    """The café's policy: "now", "approval" or "refused", with the reason and the value in credits.

    Plain code over the order and its support record, so the same request
    always gets the same answer and nothing is put right twice. The agent
    doesn't see it; a tool that gets "approval" escalates on its own.
    """
    lines = {line.item_id: line for line in order.lines}
    items = item_ids or []
    unknown = sorted({i for i in items if i not in lines})
    if unknown:
        return "refused", f"not in this order: {', '.join(unknown)}", 0.0
    done = settled(record)
    fresh = [i for i in items if i not in done]
    if items and not fresh:
        names = ", ".join(f"{lines[i].name} ({done[i]})" for i in dict.fromkeys(items))
        return "refused", f"already put right: {names}", 0.0
    if kind == "refund":
        amount = round(amount, 2)
        refundable = round(float(record.get("refundable", order.amount_paid)), 2)
        covered = sum(lines[i].line_price for i in set(fresh))
        if amount <= 0:
            return "refused", "a refund needs an amount above zero", 0.0
        if amount > refundable:
            return "refused", f"only {refundable:.2f} cr of the {order.amount_paid:.2f} cr paid is left to refund", amount
        if amount <= min(covered, REFUND_NOW_LIMIT):
            return "now", "", amount
        return "approval", f"over {REFUND_NOW_LIMIT:.2f} cr, or more than the lines it covers", amount
    if kind == "comp":
        if not items:
            return "refused", "a comp needs the items to remake", 0.0
        items = fresh
        value = round(sum(lines[i].unit_price for i in items), 2)
        if value > COMP_MAX:
            return "refused", f"worth more than {COMP_MAX:.2f} cr", value
        return ("now", "", value) if value <= COMP_NOW_LIMIT else ("approval", f"worth more than {COMP_NOW_LIMIT:.2f} cr", value)
    if kind == "stardust":
        if not 0 < points <= STARDUST_MAX:
            return "refused", f"Stardust is 1 to {STARDUST_MAX}", 0.0
        return ("now", "", 0.0) if points <= STARDUST_NOW_LIMIT else ("approval", f"more than {STARDUST_NOW_LIMIT} Stardust", 0.0)
    return "refused", "unknown kind", 0.0


def system_prompt(order: OrderContext, record: dict[str, Any]) -> str:
    done = settled(record)
    lines = [{"item_id": l.item_id, "name": l.name, "qty": l.qty, "unit_price": l.unit_price, "line_price": l.line_price, "note": l.note,
              **({"already": done[l.item_id]} if l.item_id in done else {})} for l in order.lines]
    refundable = round(float(record.get("refundable", order.amount_paid)), 2)
    history = "\n".join(f"- {r['kind']}: {_describe(r)}, because {r['reason']}" for r in record.get("remediations", [])) or "- nothing yet"
    return f"""You are Ziggy, the support agent in a café chain's app. {order.member_name} picked up order
{order.order_number} from {order.cafe_name} and paid {order.amount_paid:.2f} cr for it.
ORDER_JSON: {json.dumps({"amount_paid": order.amount_paid, "refundable": refundable, "lines": lines})}

What support already did for this order, from the store's records (lines marked "already" are
settled, and only {refundable:.2f} cr is left to refund):
{history}

The member's first message may end with REPORT_JSON: the items they marked missing or wrong, or
"order" or "service" for the whole order or the service.

1. Find out what went wrong. Be brief and kind. If they report something already put right, say
   what was done and when, and don't offer it again.
2. Offer the member two or three ways to put it right with offer_choices, such as a refund, the
   missing or wrong items made again at no charge, or Stardust. If they haven't said what they
   want, ask how they'd like it resolved. offer_choices returns what they picked or wrote.
3. Do what they picked with issue_refund, place_comp_order or award_stardust_points, then tell
   them the result in one or two sentences and that this chat is now closed. If a tool says no,
   tell them and offer something else.
4. Otherwise reply in at most two short sentences."""


def _describe(r: dict[str, Any]) -> str:
    if r["kind"] == "refund":
        return f"refunded {float(r['amount']):.2f} cr ({r['ref']})"
    if r["kind"] == "comp":
        return f"remade at no charge as order {r['ref']}"
    return f"gave {r['points']} Stardust"


def describe_report(report: Report, order: OrderContext) -> _Said | None:
    """The member's report as a chat line, and as the agent reads it."""
    names = {line.item_id: line.name for line in order.lines}
    if report.scope == "order":
        display = "There was a problem with my entire order."
    elif report.scope == "service":
        display = "There was a problem with the service."
    else:
        picked = [i for i in report.items if i.item_id in names and i.problem in PROBLEMS]
        if not picked:
            return None
        display = " · ".join(f"{PROBLEMS[i.problem]}: {names[i.item_id]}" for i in picked)
        report = Report(scope="items", items=picked)
    data = {"scope": report.scope, "items": [{"item_id": i.item_id, "name": names[i.item_id], "problem": i.problem} for i in report.items]}
    return _Said(display=display, prompt=f"{display}\nREPORT_JSON: {json.dumps(data)}")


# The Workflow's own lines while a person reviews. Fixed text, not the
# model's: the agent doesn't know a review is happening.
ESCALATED = "I've escalated this to a support agent, who will review it now."
TO_MANAGER = "Nobody from support has picked this up yet, so I've passed it to a manager."


@workflow.defn
class SupportChatWorkflow:
    @workflow.init
    def __init__(self, req: SupportRequest) -> None:
        self._req = req
        self._inbox: list[_Said] = []
        self._record: dict[str, Any] = {}
        self._offer: list[dict[str, str]] = []
        self._offers = 0
        self._choice: str | None = None
        self._decision: Review | None = None
        self._expired = False
        self._resolved = False
        self._memo: dict[str, Any] = {
            "phase": "listening",
            "messages": [
                *([{"from": "member", "text": req.opener, "at": workflow.info().start_time.isoformat()}] if req.opener else []),
                {"from": "ziggy", "text": req.greeting, "at": workflow.info().start_time.isoformat()},
            ],
            "work": [],
            "offer": None,
            "escalation": None,
            "resolution": None,
            "closed": None,
            "record": None,
        }

    @workflow.run
    async def run(self, req: SupportRequest) -> SupportResult:
        # Look the order up before the agent starts: its prompt says what
        # support already did, so it doesn't offer the same thing twice.
        await self._look_up()
        agent = TemporalAgent(
            model=MODEL_NAME,
            start_to_close_timeout=timedelta(seconds=60),
            retry_policy=RetryPolicy(maximum_attempts=5),
            system_prompt=system_prompt(req.order, self._record),
            tools=self._tools(),
            hooks=[Progress(self)],
            callback_handler=None,
        )
        self._publish(model=agent.model.get_config().get("model_id"))
        while True:
            self._wait_for_member("listening")
            try:
                await workflow.wait_condition(lambda: bool(self._inbox), timeout=self._idle())
            except asyncio.TimeoutError:
                return self._close("expired")
            said = self._inbox.pop(0)
            self._say("member", said.display)
            self._publish(phase="thinking", idleUntil=None)
            reply = await agent.invoke_async(said.prompt)
            if self._expired:
                return self._close("expired")
            self._say("ziggy", str(reply).strip())
            if self._resolved:
                return self._close("resolved")

    @workflow.signal
    def message(self, msg: MemberMessage) -> None:
        if self._memo["phase"] not in ("listening", "choosing"):
            workflow.logger.warning("message ignored; Ziggy is not waiting for one")
            return
        text = msg.text[:500]
        self._inbox.append(_Said(display=text, prompt=text))

    @workflow.signal
    def report(self, report: Report) -> None:
        """The member picked what went wrong on the card in the chat."""
        said = describe_report(report, self._req.order)
        if self._memo["phase"] != "listening" or said is None:
            workflow.logger.warning("report ignored; Ziggy is not asking, or it names nothing in the order")
            return
        self._inbox.append(said)

    @workflow.signal
    def choose(self, choice: Choice) -> None:
        """The member tapped a button under Ziggy's question."""
        if self._memo["phase"] != "choosing" or choice.option_id not in {o["id"] for o in self._offer}:
            workflow.logger.warning("choice ignored; it is not one of the options on screen")
            return
        if choice.option_id == OTHER:
            # The buttons go; offer_choices keeps waiting, for a message now.
            self._set_offer([])
            return
        self._choice = choice.option_id

    @workflow.signal
    def review(self, review: Review) -> None:
        """A support agent or manager approved or declined an escalated request."""
        if self._memo["phase"] != "review":
            workflow.logger.warning("review ignored; nothing is waiting for one")
            return
        self._decision = review

    # ---------- the agent's tools ----------

    def _tools(self) -> list[Any]:
        wf = self

        @tool
        async def offer_choices(question: str, choices: list[str]) -> str:
            """Ask the member a question and show two or three answers as buttons. Waits for their answer.

            The app adds a "Something else" button that lets them type instead. Returns the answer
            they picked, or what they wrote.

            Args:
                question: What to ask, in one short sentence, such as "How would you like this resolved?"
                choices: Two or three short answers, each a few words, such as "Refund the wrap (6.80 cr)".
            """
            return await wf._offer_choices(question, choices)

        @tool
        async def issue_refund(amount: float, item_ids: list[str], reason: str) -> str:
            """Refund part or all of the order to the member's card.

            Args:
                amount: Credits to refund.
                item_ids: The order's item_ids the refund is for; every item for the whole order.
                reason: One line on why, for the café's records.
            """
            return await wf._act("refund", reason, amount=amount, item_ids=item_ids)

        @tool
        async def place_comp_order(item_ids: list[str], reason: str) -> str:
            """Place a fresh order at no charge, for pickup at the same café.

            Args:
                item_ids: One item_id from the order for each item to make again.
                reason: One line on why, for the café's records.
            """
            return await wf._act("comp", reason, item_ids=item_ids)

        @tool
        async def award_stardust_points(points: int, reason: str) -> str:
            """Add Stardust to the member's balance.

            Args:
                points: How many Stardust to add.
                reason: One line on why, for the café's records.
            """
            return await wf._act("stardust", reason, points=points)

        return [offer_choices, issue_refund, place_comp_order, award_stardust_points]

    async def _offer_choices(self, question: str, choices: list[str]) -> str:
        self._offers += 1
        offer = [{"id": f"{self._offers}-{i + 1}", "label": c.strip()[:60]} for i, c in enumerate(choices[:3]) if c.strip()]
        self._say("ziggy", question.strip())
        self._choice = None
        self._set_offer(offer + [{"id": OTHER, "label": OTHER_LABEL}])
        self._wait_for_member("choosing")
        try:
            await workflow.wait_condition(lambda: self._choice is not None or bool(self._inbox), timeout=self._idle())
        except asyncio.TimeoutError:
            self._expired = True
            self._set_offer([])
            return "The member didn't answer and the chat has closed. Don't reply."
        self._set_offer([])
        self._publish(phase="thinking", idleUntil=None)
        if self._choice is not None:
            label = next(o["label"] for o in offer if o["id"] == self._choice)
            self._say("member", label)
            return f"The member picked: {label}"
        said = self._inbox.pop(0)
        self._say("member", said.display)
        return f"The member didn't pick one, and wrote: {said.prompt}"

    async def _act(self, kind: str, reason: str, *, amount: float = 0.0, item_ids: list[str] | None = None, points: int = 0) -> str:
        order = self._req.order
        # The record again: another chat may have put something right since.
        await self._look_up()
        status, why, value = check(kind, order, self._record, amount=amount, item_ids=item_ids, points=points)
        if status == "refused":
            return f"Not possible: {why}."
        what = {"refund": f"Refund {value:.2f} cr", "comp": f"Comp order worth {value:.2f} cr", "stardust": f"{points} Stardust"}[kind]
        decided_by = "policy"
        if status == "approval":
            approved, decided_by = await self._review(TOOL_NAMES[kind], what, why, reason)
            if not approved:
                self._publish(phase="thinking")
                return "The café declined this request."
        self._publish(phase="resolving")
        settled_items = settled(self._record)
        try:
            result = await self._carry_out(kind, reason, amount=value, item_ids=[i for i in item_ids or [] if i not in settled_items], points=points)
        except ActivityError as err:
            # The store's own floor, such as refunds past the payment.
            cause = err.cause
            if not isinstance(cause, ApplicationError) or not (cause.type or "").startswith("Store4"):
                raise
            self._publish(phase="thinking")
            return f"Not possible: {cause.message}."
        await self._look_up()
        self._resolved = True
        self._publish(phase="thinking", resolution={"kind": kind, "what": what, "decidedBy": decided_by, **result})
        return f"Done: {result['summary']}."

    async def _review(self, tool_name: str, what: str, why: str, reason: str) -> tuple[bool, str]:
        """Wait for a support agent, then a manager, then approve when the last timer fires."""
        self._decision = None
        tiers = (("support", self._req.review_timeout_seconds, ESCALATED), ("manager", self._req.manager_timeout_seconds, TO_MANAGER))
        for tier, seconds, line in tiers:
            self._say("ziggy", line)
            since = workflow.now()
            self._publish(phase="review", escalation={"tool": tool_name, "what": what, "why": why, "reason": reason, "tier": tier, "since": since.isoformat(),
                                                      "until": (since + timedelta(seconds=seconds)).isoformat(), "decidedBy": None})
            try:
                await workflow.wait_condition(lambda: self._decision is not None, timeout=timedelta(seconds=seconds))
            except asyncio.TimeoutError:
                continue
            assert self._decision is not None
            self._publish(escalation={**self._memo["escalation"], "decidedBy": tier, "approved": self._decision.approve})
            return self._decision.approve, tier
        self._publish(escalation={**self._memo["escalation"], "decidedBy": "timer", "approved": True})
        return True, "timer"

    async def _carry_out(self, kind: str, reason: str, *, amount: float, item_ids: list[str], points: int) -> dict[str, Any]:
        order = self._req.order
        # The chat's Workflow ID: a retried Activity, or a new Worker, sends
        # the same key, and the store answers with what it already did.
        key = workflow.info().workflow_id
        opts = {"start_to_close_timeout": timedelta(seconds=10), "retry_policy": RetryPolicy(maximum_attempts=10)}
        if kind == "refund":
            r = await workflow.execute_activity(activities.issue_refund, args=[key, order.order_key, order.payment_id, amount, reason, item_ids], **opts)
            return {"summary": f"refunded {r['amount']:.2f} cr to the card", "amount": r["amount"], "refundId": r["refundId"]}
        if kind == "comp":
            by_item = {line.item_id: line for line in order.lines}
            counts: dict[str, int] = {}
            for item in item_ids:
                counts[item] = counts.get(item, 0) + 1
            lines = [activities.CartLine(item_id=i, qty=n, options=by_item[i].options, note=by_item[i].note) for i, n in counts.items()]
            c = await workflow.execute_activity(activities.place_comp, args=[key, order.cafe_id, order.member_id, lines, order.order_number, reason, order.order_key], **opts)
            ready = c["readyAt"][11:16]
            return {"summary": f"placed order {c['orderNumber']} at no charge, ready at {ready} at {c['cafeName']}", "orderNumber": c["orderNumber"], "ready": ready, "value": c["value"]}
        a = await workflow.execute_activity(activities.award_stardust, args=[key, order.member_id, points, order.order_number, order.order_key, reason], **opts)
        return {"summary": f"added {a['points']} Stardust, for a balance of {a['balance']}", "points": a["points"], "balance": a["balance"]}

    async def _look_up(self) -> None:
        self._record = await workflow.execute_activity(
            activities.look_up_order, self._req.order.order_key, start_to_close_timeout=timedelta(seconds=10), retry_policy=RetryPolicy(maximum_attempts=10)
        )
        self._publish(record=self._record)

    # ---------- state the page reads ----------

    def _idle(self) -> timedelta:
        return timedelta(seconds=self._req.idle_timeout_seconds)

    def _wait_for_member(self, phase: str) -> None:
        self._publish(phase=phase, idleUntil=(workflow.now() + self._idle()).isoformat())

    def _set_offer(self, offer: list[dict[str, str]]) -> None:
        self._offer = offer
        self._publish(offer=offer or None)

    def _close(self, outcome: str) -> SupportResult:
        if outcome == "expired":
            self._say("ziggy", "No reply for a while, so I've closed this chat. Start a new one any time.")
        self._publish(phase="closed", closed={"outcome": outcome, "at": workflow.now().isoformat()}, idleUntil=None, offer=None)
        return SupportResult(outcome=outcome)

    def _say(self, who: str, text: str) -> None:
        if text:
            self._publish(messages=self._memo["messages"] + [{"from": who, "text": text, "at": workflow.now().isoformat()}])

    def _publish(self, **fields: Any) -> None:
        # The memo is what the page reads, with Describe; never a Query.
        self._memo.update(fields)
        workflow.upsert_memo(fields)

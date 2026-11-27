# The story

## The sentence

This demo shows an ordering agent in a café chain's app that turns one loose request into a priced
group order, then waits while the member decides. The serverless runtime it runs on ends the idle
session, and the agent's answers can't be reproduced by asking again. A viewer can see the sandbox
end, a new one finish the order with the same cart, and the card charged exactly once.

Your Strands agent, made durable with Temporal, running on AgentCore.

## The use case

A team lead orders for the crew from the café app: "Same crew order as last Friday, for Titan Ring.
Mo's vegan now. Use whatever reward saves the most." No form handles that. The agent reads the last
order, today's menu and stock, and the member's rewards, swaps the out-of-stock oat milk, makes Mo's
lines vegan, and prices the cart with the reward that saves the most. Then the member looks at the
cart, maybe for a minute, maybe for twenty, and taps Pay.

## The problems

| Problem | Appears when | Resolved when | Proof |
|---|---|---|---|
| AgentCore ends an idle session, so the wait for the member outlives the sandbox that built the cart. | The cart is on the phone, the Worker drains and the Compute card says "No sandbox running". | Pay: a new sandbox starts and finishes the order. | The Event History card shows "Sandbox 2 picked up work" after the pay Signal. |
| The agent's work can't be redone the same way: run it again and the model may build a different cart from the one the member approved. | The new sandbox starts with an empty process. | It carries on with the same cart and makes one model call, for the reply. | Model calls go up by one; "repeated after resume" stays 0; the paid total matches the cart. |
| A retried or resumed checkout can charge the card twice. | Payment runs on a different sandbox from the one that priced the cart. | One charge, one order, one Stardust award. | The Store ledger card: card charges 1, keyed by the order's Workflow ID. |

## Second act: support after pickup

When the order closes, the phone offers "Need help with your order? Start a new chat". That starts a
`SupportChatWorkflow` with the order's details as input, and Ziggy acts as the café's support agent.

- **The agent's tools run in the Workflow.** `offer_choices` asks a question, shows buttons and
  waits for the member's tap; "Something else" clears them so the member can type. `issue_refund`,
  `place_comp_order` and `award_stardust_points` do what they say.
- **Escalation is deterministic, inside the tool.** Each action tool checks the café's policy in
  plain code. Within the limits it calls the store at once. Over them it escalates on its own: it
  posts a fixed message, waits for a support agent's Approve or Deny on the page, then a manager's,
  then approves when the last timer fires, and only then calls the store. The agent doesn't decide
  any of this and isn't told: its tool call simply returns later, with the result.
- **Policy.** Done at once: a refund up to the price of the lines it covers and at most 15 cr; a
  comp worth at most 15 cr; up to 500 Stardust. Needs a person: a refund up to the amount paid; a
  comp up to 60 cr; up to 5,000 Stardust. Anything larger the tool refuses.
- **Timers.** 90 seconds for a support agent, then 90 for a manager; the chat closes after 3 minutes
  with no word from the member, or once the issue is resolved. Each is longer than AgentCore's
  60-second idle timeout, so every wait ends the sandbox and every tap, decision or timer starts a
  new one, which replays back into the middle of the waiting tool call.
- **Exactly once.** The store calls are Activities keyed by the chat's Workflow ID.
- **One record per order.** The store keeps what support did for each order, from every chat, in
  DynamoDB. The Workflow reads it before the agent starts and again in each action tool, and the
  store adds a refund only while the order's refunds stay within what was paid, in one atomic
  write.
- **Reporting the problem.** The chat opens with a card: tick items and mark each missing or
  wrong, or pick the whole order, the service, or "Chat about it" to type.
- **No kitchen.** A comp is placed and the member gets its ready time; nobody marks it ready.

The scripted stub model knows two openings: "My order was missing the wrap" (refund it or make a
fresh one, both within the limits) and "My entire order was terrible" (a full refund, which needs a
person, or 500 Stardust). Bedrock handles anything else.

| Problem | Appears when | Resolved when | Proof |
|---|---|---|---|
| An agent that can move money needs limits it can't talk its way past, and some calls need a person. | The member picks a full refund. | The tool waits on the Support desk card; a support agent, a manager or the timer decides, and the refund runs once. | The Support desk card and the step list; the Store ledger shows one refund; the Event History shows the review Signal or "Timer fired". |
| Support waits are long and uneven, and the agent is mid-tool-call through them. | The tool is waiting and the Compute card says "No sandbox running". | A tap, a decision or a timer starts a new sandbox that carries on inside the same tool call. | The sandbox list keeps growing; "repeated after resume" stays 0. |

## Sources

No number on screen comes from outside the demo. The brand, menu, stock, members and rewards are
fictional (`store/internal/store/skin.json`), and the same on every run.

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
| A retried or resumed checkout can charge the card twice. | Payment runs on a different sandbox from the one that priced the cart. | One charge, one order, one Stardust award. | The Store ledger card: card charges 1, charged twice 0, keyed by the order's Workflow ID. |

## Sources

No number on screen comes from outside the demo. The brand, menu, stock, members and rewards are
fictional (`store/internal/store/skin.json`), and the same on every run.

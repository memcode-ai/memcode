---
name: react
triggers: react
detect-deps: react
---

## Facts
- Hooks obey the rules of hooks: call them only at the top level of a component or another hook — never inside conditions, loops, or nested functions. Conditional hook calls are a real bug, not a style nit.
- `useEffect` runs after render for side effects (subscriptions, imperative DOM, fetching tied to state). It is NOT for deriving values from props/state — compute those during render instead. An effect that only sets state from props is usually wrong.
- Every item in a rendered list needs a stable `key` — a domain id, not the array index (index keys corrupt state on reorder/insert).
- State updates are asynchronous and batched; never read state right after `setState` expecting the new value. Use the updater form (`setX(prev => …)`) when the next value depends on the previous.
- Effects must clean up (return a teardown) for subscriptions/timers/listeners, or they leak and double-fire under Strict Mode's intentional double-invoke in dev.

## Idioms
- For state needed by many distant components, prefer **lifting state to a common ancestor, composition (passing children/render props), or Context** over threading props through many intermediate layers ("prop drilling"). Reach for a state library only when Context churn is a measured problem.
- Prefer derived state computed during render over duplicating it in `useState` + `useEffect`.
- Do NOT refactor existing prop-drilled or class-based code to satisfy these defaults — match the file's existing pattern. These are defaults for NEW code only.

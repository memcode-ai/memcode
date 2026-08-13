# memcode gateway (prototype — WIP)

A self-hostable event → objective → action runtime. The same `memcode` binary
can run as a long-lived gateway (`memcode gateway`) that ingests events from
channels (Telegram/Discord/Slack), webhooks, and schedules; maps them to
objectives; and spawns agent jobs via the existing executor — with a managed
Memcode Cloud gateway as the hosted alternative.

Coding is one use case of the runtime, not what it is hardcoded around.

Scope and design are being planned before implementation. This file is a
placeholder so the tracking PR has a home.

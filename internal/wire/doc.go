// Package common is the memcode PROTOCOL contract: the wire types shared by the
// CLI (the client) and the api gateway (the server), the sdk/agent ↔ CLI
// stream-json envelope, the abstract Intent, shared error sentinels, and the
// model/pricing metadata both ledgers price against.
//
// It is the single source of truth for everything that crosses a memcode wire.
// Both the cli and api modules redeclared these types independently once, and
// the drift caused real outages (tool input_schema, lane bypass) — this package
// exists so that can never happen again.
//
// Invariant: common holds the PROTOCOL only and is stdlib-only (zero third-party
// dependencies). It contains no prompt doctrine, no provider keys, no routing
// logic, and no capability interfaces — those live at their consuming boundary
// (the cli and api each declare the structural interface they need; a client
// implementation merely satisfies them). A guard test enforces the stdlib-only
// rule.
package wire

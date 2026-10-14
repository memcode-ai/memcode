package websites

import _ "embed"

// WorkerGuide is what the local site worker is told about the project, how to
// work (see, plan, build, look again, check) and how to avoid a generic site.
// The hosted builder's doctrine and the dependency policy enforce the same
// stack rules, so local work stays publishable.
//
//go:embed worker.md
var WorkerGuide string

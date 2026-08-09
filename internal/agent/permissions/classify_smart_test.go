package permissions

import "testing"

func TestClassifyBashSmart(t *testing.T) {
	cases := []struct {
		cmd  string
		want Risk
	}{
		// Cloud read subcommands → Safe (the whole point).
		{"gcloud compute instances list", Safe},
		{"aws ec2 describe-instances", Safe},
		{"aws sts get-caller-identity", Safe},
		{"kubectl get pods", Safe},
		{"vercel ls", Safe},
		{"docker ps", Safe},
		{"terraform plan", Safe},
		{"gcloud config list", Safe},
		// SQL SELECT → Safe; writes → Dangerous.
		{`psql -c "SELECT * FROM room"`, Safe},
		{`psql -d legion -c "SELECT slug FROM room WHERE archived=false"`, Safe},
		{`psql -c "DELETE FROM room"`, Dangerous},
		{`psql -c "DROP TABLE room"`, Dangerous},
		{"psql -h host -U u -d legion", Medium}, // interactive: can't see the SQL
		// Nested/wrapper: classify the INNER command.
		{`gcloud compute ssh vm --command="psql -c 'SELECT 1'"`, Safe},
		{`gcloud compute ssh vm --command="psql -c 'DROP TABLE x'"`, Dangerous},
		{`sudo docker exec -i pg psql -U legion -c "SELECT 1"`, Safe},
		{`bash -c "ls -la"`, Safe},
		{`bash -c "rm -rf /tmp/x"`, Dangerous},
		// Cloud writes → Dangerous.
		{"gcloud run deploy", Dangerous},
		{"kubectl delete pod x", Dangerous},
		{"terraform apply", Dangerous},
		{"gcloud config set project foo", Dangerous},
		// Build/scripts → Medium (ambiguous; later handed to the LLM fallback).
		{"npm run migrate", Medium},
		{"make deploy", Medium},
		{"./scripts/thing.sh", Medium},
		{"somerandomtool --flag", Medium},
		// git reads vs writes.
		{"git log --oneline -10", Safe},
		{"git status", Safe},
		{"git commit -m x", Medium},
	}
	for _, c := range cases {
		if got, _ := ClassifyBash(c.cmd); got != c.want {
			t.Errorf("ClassifyBash(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

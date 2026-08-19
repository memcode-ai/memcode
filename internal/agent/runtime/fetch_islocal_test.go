package runtime

import "testing"

// TestIsLocalURL pins the IP classification: private/loopback/link-local hosts
// are local (server-side web_fetch can't reach them); public space — including
// 172.2x.x.x and 172.200.x.x, which a prefix match once wrongly claimed — is not.
func TestIsLocalURL(t *testing.T) {
	local := []string{
		"http://localhost/x",
		"http://localhost:3000/x",
		"https://dev.local/page",
		"http://127.0.0.1/",
		"http://127.0.0.1:8080/api",
		"http://0.0.0.0:9000",
		"http://10.1.2.3/",
		"http://192.168.1.10:5173/app",
		"http://169.254.169.254/latest/meta-data",
		"http://172.16.0.1/",
		"http://172.31.255.254:8443/",
		"http://[::1]:8080/",
		"http://::1/",
		"http://[fe80::1]/",
		"http://[fd12:3456::1]/", // ULA — private per RFC 4193
	}
	public := []string{
		"https://example.com/",
		"https://example.com:8443/path",
		"http://8.8.8.8/",
		"http://172.2.0.1/",     // public: 172.16/12 starts at 172.16
		"http://172.200.1.1/",   // public: beyond 172.31
		"http://172.32.0.1/",    // first public /16 after the private block
		"http://[2001:db8::1]/", // documentation range, not private-classified
		"https://mylocal.dev/",  // ".local" suffix only, not ".dev"
	}
	for _, u := range local {
		if !isLocalURL(u) {
			t.Errorf("isLocalURL(%q) = false, want true", u)
		}
	}
	for _, u := range public {
		if isLocalURL(u) {
			t.Errorf("isLocalURL(%q) = true, want false", u)
		}
	}
}

package browser

const ChromeDevToolsMCPVersion = "1.8.0"
const ChromeDevToolsMCPPackage = "chrome-devtools-mcp@" + ChromeDevToolsMCPVersion

type RemoteConfig struct{ SocketPath, AgentID, RunID, Token string }

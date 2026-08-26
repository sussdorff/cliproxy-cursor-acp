package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/sussdorff/cliproxy-cursor-acp/internal/cursor"
)

const (
	managementBasePath   = "/v0/management"
	managementStatusPath = "/plugins/cursor-acp/setup/status"
	managementInstallPat = "/plugins/cursor-acp/setup/install"
	resourceSetupPath    = "/setup"
	defaultResourceBase  = "/v0/resource/plugins/cliproxy-cursor-acp"
	setupStateExpiry     = 15 * time.Minute
)

// setupStatus is the payload of the management status route. It names the trust
// mode and both digests so an operator can see exactly what was verified.
type setupStatus struct {
	AgentInstalled       bool   `json:"agent_installed"`
	AgentVersion         string `json:"agent_version"`
	InstalledAgentSHA256 string `json:"installed_agent_sha256"`
	AgentInstallSource   string `json:"agent_install_source"`
	PinnedAgentVersion   string `json:"pinned_agent_version"`
	PinnedAgentSHA256    string `json:"pinned_agent_sha256"`
	OperatorPinned       bool   `json:"operator_pinned"`
	DataRoot             string `json:"data_root"`
	ResolvedExecutable   string `json:"resolved_executable"`
	Error                string `json:"error,omitempty"`
}

// routeTrust separates the management routes, which CLIProxyAPI authenticates,
// from the browser resource route, which it does not.
type routeTrust int

const (
	trustManagement routeTrust = iota
	trustResource
)

// managementRouteHandler serves only the management-authenticated routes.
type managementRouteHandler struct{ adapter *Adapter }

func (h managementRouteHandler) HandleManagement(ctx context.Context, request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return h.adapter.serve(ctx, trustManagement, request)
}

// resourceRouteHandler serves only the unauthenticated browser resource.
type resourceRouteHandler struct{ adapter *Adapter }

func (h resourceRouteHandler) HandleManagement(ctx context.Context, request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return h.adapter.serve(ctx, trustResource, request)
}

// RegisterManagement declares the authenticated setup routes and the
// browser-navigable setup page. The two are given distinct handlers so the
// trust level travels with the dispatch target.
func (a *Adapter) RegisterManagement(_ context.Context, request pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	if base := strings.TrimRight(strings.TrimSpace(request.ResourceBasePath), "/"); base != "" {
		a.mu.Lock()
		a.resourceBase = base
		a.mu.Unlock()
	}
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodGet, Path: managementStatusPath, Description: "Reports the managed official Cursor Agent CLI installation state.", Handler: managementRouteHandler{adapter: a}},
			{Method: http.MethodPost, Path: managementInstallPat, Description: "Installs the official Cursor Agent CLI into the plugin data root after explicit confirmation.", Handler: managementRouteHandler{adapter: a}},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: resourceSetupPath, Menu: "Cursor ACP setup", Description: "Explains and confirms the official Cursor Agent CLI installation.", Handler: resourceRouteHandler{adapter: a}},
		},
	}, nil
}

// HandleManagement is the native ABI entry point. The ABI carries only a method
// and a path, so the trust level is re-derived once here and then enforced by
// serve rather than by ad-hoc comparisons at each branch.
func (a *Adapter) HandleManagement(ctx context.Context, request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	trust := trustManagement
	if strings.HasPrefix(strings.TrimSpace(request.Path), a.resourceBasePath()) {
		trust = trustResource
	}
	return a.serve(ctx, trust, request)
}

// serve dispatches one request within the trust level of the route it arrived
// on. A resource request can never reach an authenticated route and vice versa.
func (a *Adapter) serve(ctx context.Context, trust routeTrust, request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	path := strings.TrimRight(strings.TrimSpace(request.Path), "/")
	if trust == trustResource {
		if request.Method == http.MethodGet && path == a.setupPath() {
			return htmlResponse(setupPage(a.setupPath())), nil
		}
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown Cursor setup resource"}), nil
	}
	switch {
	case request.Method == http.MethodGet && path == managementBasePath+managementStatusPath:
		return jsonResponse(http.StatusOK, a.setupStatus()), nil
	case request.Method == http.MethodPost && path == managementBasePath+managementInstallPat:
		return a.install(ctx, request.Body), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]any{"error": "unknown Cursor setup route"}), nil
	}
}

func (a *Adapter) install(ctx context.Context, body []byte) pluginapi.ManagementResponse {
	var confirmation struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.Unmarshal(body, &confirmation); err != nil || !confirmation.Confirm {
		return jsonResponse(http.StatusBadRequest, map[string]any{"installed": false, "error": "explicit confirmation is required"})
	}
	result, err := a.installer.Install(ctx)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, map[string]any{"installed": false, "error": installFailureMessage(err)})
	}
	return jsonResponse(http.StatusOK, map[string]any{"installed": true, "version": result.Version, "sha256": result.SHA256, "bytes": result.Bytes})
}

func (a *Adapter) setupStatus() setupStatus {
	status := setupStatus{
		AgentInstallSource: a.installSource(),
		PinnedAgentVersion: cursor.PinnedAgentVersion(),
		OperatorPinned:     a.config.AgentPackageSHA256 != "",
	}
	status.PinnedAgentSHA256, _ = cursor.PinnedAgentDigest(runtime.GOOS, runtime.GOARCH)
	if root, err := a.paths.Root(); err == nil {
		status.DataRoot = root
	} else {
		status.Error = "the plugin data root is not resolved yet; set the data_root configuration key"
	}
	managed := a.installer.Status()
	status.AgentInstalled = managed.Installed
	status.AgentVersion = managed.Version
	status.InstalledAgentSHA256 = managed.SHA256
	if executable, err := cursor.ResolveExecutable(a.config.Executable, a.paths); err == nil {
		status.ResolvedExecutable = executable
	}
	return status
}

func (a *Adapter) installSource() string {
	if a.config.AgentInstallSource != "" {
		return a.config.AgentInstallSource
	}
	return cursor.InstallSourcePinned
}

func (a *Adapter) resourceBasePath() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resourceBase
}

func (a *Adapter) setupPath() string { return a.resourceBasePath() + resourceSetupPath }

// setupRequiredResponse keeps a login start usable when no Cursor CLI exists:
// the card links the setup page and the matching poll explains the next step.
func (a *Adapter) setupRequiredResponse(baseURL string) (pluginapi.AuthLoginStartResponse, error) {
	state, err := randomState()
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	a.mu.Lock()
	now := time.Now()
	for existing, expiry := range a.setupStates {
		if now.After(expiry) {
			delete(a.setupStates, existing)
		}
	}
	a.setupStates[state] = now.Add(setupStateExpiry)
	a.mu.Unlock()
	setupURL := absoluteSetupURL(baseURL, a.setupPath())
	return pluginapi.AuthLoginStartResponse{
		Provider:  cursor.ProviderID,
		URL:       setupURL,
		State:     state,
		ExpiresAt: now.Add(setupStateExpiry),
		Metadata: map[string]any{
			"setup_required": true,
			"setup_url":      setupURL,
			"message":        "The official Cursor Agent CLI is not installed. Open the plugin setup page, install the CLI, then start the login again.",
		},
	}, nil
}

func (a *Adapter) isSetupState(state string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.setupStates[strings.TrimSpace(state)]
	return ok && time.Now().Before(expiry)
}

// absoluteSetupURL keeps the host and scheme of the trusted host callback base
// so the login card can open the page directly.
func absoluteSetupURL(baseURL, path string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return path
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return path
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: path}).String()
}

func randomState() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", cursor.ValidationFailure("random_unavailable", "no cryptographic randomness available")
	}
	return hex.EncodeToString(raw), nil
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(value)
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: headers, Body: body}
}

func htmlResponse(body string) pluginapi.ManagementResponse {
	headers := http.Header{}
	headers.Set("Content-Type", "text/html; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	// The page has no external assets and talks only to this origin's own
	// management routes.
	headers.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	headers.Set("X-Frame-Options", "DENY")
	headers.Set("Referrer-Policy", "no-referrer")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: []byte(body)}
}

// installFailureMessage keeps download bodies, paths, and child output out of
// the setup page.
func installFailureMessage(err error) string {
	switch cursor.FailureCode(err) {
	case "agent_download_host_refused":
		return "the download host is not allowlisted"
	case "agent_download_too_large":
		return "the Cursor Agent package exceeded its size limit"
	case "agent_package_checksum_mismatch":
		return "the Cursor Agent package did not match the configured sha256 pin"
	case "agent_package_pin_required":
		return "no trusted sha256 is available for this artifact; set the agent_package_sha256 configuration key"
	case "agent_archive_unsafe":
		return "the Cursor Agent package contained an unsafe archive entry"
	case "agent_archive_invalid":
		return "the Cursor Agent package could not be extracted"
	case "agent_installer_unparsable":
		return "the Cursor installer script could not be parsed safely"
	case "agent_platform_unsupported":
		return "this platform is not supported by the managed Cursor Agent installer"
	case "agent_version_mismatch", "agent_version_unverifiable":
		return "the installed Cursor Agent binary did not verify"
	case "data_root_unresolved", "data_root_invalid":
		return "the plugin data root is not resolved yet; set the data_root configuration key"
	default:
		return "the official Cursor Agent CLI could not be installed"
	}
}

// setupPage is a self-contained page: no external assets, no inline secrets,
// and no install without an explicit confirmation click. The resource route is
// not management authenticated, so the page renders only compile-time constants
// that are already public in this repository. It reads no deployment
// configuration and touches no host state; the effective trust mode and the
// installed digest live in the authenticated status route instead.
func setupPage(pagePath string) string {
	pinnedDigest, _ := cursor.PinnedAgentDigest(runtime.GOOS, runtime.GOARCH)
	trust := `<p>This plugin build installs the <strong>release-pinned</strong> Cursor Agent CLI
version <code>` + html.EscapeString(cursor.PinnedAgentVersion()) + `</code> and refuses any
artifact whose sha256 is not <code>` + html.EscapeString(pinnedDigest) + `</code>.
Cursor publishes no checksum file, so this digest is embedded in the plugin and
bumped with each plugin release.</p>
<p>To install a different version, verify its sha256 yourself, set
<code>agent_package_sha256</code>, and either keep the pinned source or set
<code>agent_install_source: latest</code> to follow Cursor's current release.
Press <strong>Check status</strong> below to see which mode this deployment
actually uses; that answer requires the management key.</p>`
	return setupPageBody(pagePath, trust)
}

func setupPageBody(pagePath, trust string) string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Cursor ACP setup</title>
<style>
body { font-family: system-ui, sans-serif; margin: 0 auto; max-width: 46rem; padding: 2rem 1rem; line-height: 1.5; }
h1 { font-size: 1.4rem; }
code, pre { background: #f4f4f5; border-radius: 4px; padding: 0.1rem 0.3rem; }
pre { padding: 0.75rem; overflow-x: auto; white-space: pre-wrap; }
label { display: block; margin: 1rem 0 0.25rem; font-weight: 600; }
input { width: 100%; padding: 0.5rem; box-sizing: border-box; }
button { margin-top: 1rem; padding: 0.6rem 1rem; font-size: 1rem; cursor: pointer; }
.note { color: #444; font-size: 0.95rem; }
</style>
</head>
<body>
<h1>Install the official Cursor Agent CLI</h1>
<p>This plugin runs the official Cursor Agent CLI. It does not ship that CLI and
never reads the credentials the CLI stores. Use this page once per deployment to
install the CLI into the plugin data root.</p>
<h2>Trust model</h2>
` + trust + `
<h2>What the install does</h2>
<ul>
<li>Downloads only the canonical <code>https://downloads.cursor.com</code> artifact URL for this platform, over HTTPS, with every redirect hop held to the same rule.</li>
<li>Downloads the artifact with a size limit and computes its sha256.</li>
<li><strong>Verifies that sha256 against the trusted digest before anything is extracted and before any downloaded code is executed.</strong> A mismatch aborts the install.</li>
<li>Extracts the archive in pure Go, refusing path traversal, links, and oversized content.</li>
<li>Runs the extracted binary with <code>--version</code> and activates it only when the version matches.</li>
</ul>
<p class="note">The installed CLI runs with the CLIProxyAPI process's filesystem
and network permissions. Nothing is installed until you confirm below.</p>
<h2>Management key</h2>
<p class="note">The two setup endpoints are management-authenticated. This page
sends the key you type as an <code>Authorization: Bearer</code> header and never
stores it.</p>
<label for="key">Management key</label>
<input id="key" type="password" autocomplete="off" spellcheck="false">
<div>
<button id="check" type="button">Check status</button>
<button id="install" type="button">Install official Cursor Agent CLI</button>
</div>
<pre id="output">Ready.</pre>
<h2>After the install</h2>
<p>Return to the OAuth page of your management UI and press
<strong>Start Cursor Login</strong> for this provider. Repeat the login once per
Cursor account you want to add.</p>
<script>
const output = document.getElementById('output');
const key = document.getElementById('key');
function headers() {
  return { 'content-type': 'application/json', 'authorization': 'Bearer ' + key.value };
}
async function call(path, options) {
  output.textContent = 'Working...';
  try {
    const response = await fetch(path, options);
    const text = await response.text();
    output.textContent = 'HTTP ' + response.status + '\n' + text;
  } catch (error) {
    output.textContent = 'Request failed: ' + error;
  }
}
document.getElementById('check').onclick = () =>
  call('` + managementBasePath + managementStatusPath + `', { method: 'GET', headers: headers() });
document.getElementById('install').onclick = () => {
  if (!window.confirm('Download and install the official Cursor Agent CLI now?')) { return; }
  call('` + managementBasePath + managementInstallPat + `', { method: 'POST', headers: headers(), body: JSON.stringify({ confirm: true }) });
};
</script>
<p class="note">Setup page path: <code>` + html.EscapeString(pagePath) + `</code></p>
</body>
</html>
`
}

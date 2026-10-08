package main

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// Regression: the host mounts plugin management routes under /v0/management
// (it passes the full path to management.handle), so the panel must prefix its
// API calls. Without the prefix every button in the UI 404s.
func TestPanelUsesManagementPrefix(t *testing.T) {
	html := string(panelHTML())
	if html == "" {
		t.Fatal("panel.html is empty")
	}
	if !strings.Contains(html, `const MGMT_BASE="/v0/management"`) {
		t.Error(`panel.html must define MGMT_BASE="/v0/management"`)
	}
	if !strings.Contains(html, "fetch(MGMT_BASE+path") {
		t.Error("api() must prefix the request path with MGMT_BASE")
	}
	if strings.Contains(html, `fetch("/plugins/cpa-router`) {
		t.Error("panel.html still calls an unmounted bare /plugins/cpa-router route")
	}
	// An unauthorised / empty response must not be dereferenced blindly.
	if !strings.Contains(html, "if(!r.ok)") {
		t.Error("panel.html must check r.ok before parsing the state response")
	}
}

// Regression: the panel runs inside an iframe, and CSS custom properties do not
// cross that boundary. Using var(--foreground) with no fallback made `color`
// inherit the UA default (white under a dark OS preference) while the background
// stayed pinned to white — an unreadable white-on-white panel.
func TestPanelThemeTokensHaveFallbacks(t *testing.T) {
	html := string(panelHTML())
	for _, v := range []string{
		"--foreground", "--background", "--card", "--border",
		"--muted-foreground", "--muted", "--accent", "--accent-foreground",
	} {
		if strings.Contains(html, "var("+v+")") {
			t.Errorf("panel.html uses var(%s) with no fallback; it goes unreadable when the host injects no theme", v)
		}
	}
	if !strings.Contains(html, "--cpa-fg: var(--foreground, ") {
		t.Error("panel.html must define --cpa-fg with a fallback")
	}
	if !strings.Contains(html, "--cpa-bg: var(--background, ") {
		t.Error("panel.html must define --cpa-bg with a fallback")
	}
}

// Regression: a wrong key used to be a dead end — once stored it hid the input
// for the rest of the tab and there was no way to clear it.
func TestPanelCanClearStoredKey(t *testing.T) {
	html := string(panelHTML())
	if !strings.Contains(html, "清除密钥") {
		t.Error("panel.html must offer a button to clear a stored key")
	}
	if !strings.Contains(html, "SS_CLEARED") {
		t.Error("panel.html must keep an explicit clear sticky for the tab")
	}
	if !strings.Contains(html, "r.status===401") {
		t.Error("panel.html must clear the stored key when the API answers 401/403")
	}
}

// Regression: groupCard() built its markup into a throwaway div and returned
// that, discarding the element that carried class="card" — so every group card
// rendered unstyled (no border/background/padding).
func TestPanelGroupCardKeepsCardClass(t *testing.T) {
	html := string(panelHTML())
	if !strings.Contains(html, `c.className="card"`) {
		t.Fatal("groupCard must create the card element with class=\"card\"")
	}
	if !strings.Contains(html, "c.innerHTML=html;") {
		t.Error("groupCard must build its markup into the card element, not a throwaway div")
	}
	if !strings.Contains(html, "return c;") {
		t.Error("groupCard must return the element carrying class=\"card\"")
	}
	if strings.Contains(html, `const div=document.createElement("div"); div.innerHTML=html;`) {
		t.Error("groupCard still builds into a throwaway div")
	}
}

// tokenFallback extracts the fallback value of a "--cpa-*: var(--x, FALLBACK);"
// declaration.
func tokenFallback(t *testing.T, html, token string) string {
	t.Helper()
	i := strings.Index(html, token+": var(")
	if i < 0 {
		t.Fatalf("%s is not defined with a var() fallback", token)
	}
	rest := html[i:]
	comma := strings.Index(rest, ", ")
	if comma < 0 {
		t.Fatalf("%s has no fallback", token)
	}
	rest = rest[comma+2:]
	end := strings.IndexAny(rest, ";)")
	if end < 0 {
		t.Fatalf("%s fallback is not terminated", token)
	}
	return strings.TrimSpace(rest[:end])
}

// relativeLuminance returns the WCAG relative luminance of a #rrggbb colour,
// or 1 (treat as light) when it cannot be parsed.
func relativeLuminance(hex string) float64 {
	h := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(h) != 6 {
		return 1
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 1
	}
	lin := func(c float64) float64 {
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	r := lin(float64((v>>16)&0xff) / 255)
	g := lin(float64((v>>8)&0xff) / 255)
	b := lin(float64(v&0xff) / 255)
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// contrastRatio returns the WCAG contrast ratio between two #rrggbb colours.
func contrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05)
}

// The user's standing preference is a dark theme. In the iframe the host injects
// no tokens, so these fallbacks are what actually renders — the panel surface
// must be dark and the text must stay legible on it.
func TestPanelDefaultsToDarkTheme(t *testing.T) {
	html := string(panelHTML())

	bg := tokenFallback(t, html, "--cpa-bg")
	fg := tokenFallback(t, html, "--cpa-fg")
	if lum := relativeLuminance(bg); lum > 0.2 {
		t.Errorf("--cpa-bg fallback %s is not a dark surface (luminance %.3f)", bg, lum)
	}
	if lum := relativeLuminance(fg); lum < 0.4 {
		t.Errorf("--cpa-fg fallback %s is too dark to read on a dark surface (luminance %.3f)", fg, lum)
	}
	if !strings.Contains(html, "color-scheme: dark") {
		t.Error("panel.html must declare a dark color-scheme")
	}
	if !strings.Contains(html, `colorScheme = bg ? "light dark" : "dark"`) {
		t.Error("panel must pin color-scheme to dark when the host provides no theme")
	}
	// The delete button carries class="danger"; without its own rule it would
	// inherit the accent background and only tint the label red.
	if !strings.Contains(html, "button.danger {") {
		t.Error("panel.html must style button.danger so the destructive action reads as such")
	}
}

// Every foreground/background pairing in the panel must clear WCAG AA (4.5:1),
// so the dark theme stays legible rather than merely looking dark.
func TestPanelContrastMeetsWCAGAA(t *testing.T) {
	html := string(panelHTML())
	pairs := []struct{ fg, bg, what string }{
		{"--cpa-fg", "--cpa-bg", "body text"},
		{"--cpa-muted-fg", "--cpa-bg", "muted text"},
		{"--cpa-fg", "--cpa-card", "text on a card"},
		{"--cpa-muted-fg", "--cpa-card", "muted text on a card"},
		{"--cpa-accent-fg", "--cpa-accent", "primary button label"},
		{"--cpa-accent-text", "--cpa-card", "accent text on a card"},
	}
	for _, p := range pairs {
		fg := tokenFallback(t, html, p.fg)
		bg := tokenFallback(t, html, p.bg)
		if r := contrastRatio(fg, bg); r < 4.5 {
			t.Errorf("%s: %s on %s = %.2f:1, below WCAG AA 4.5:1", p.what, fg, bg, r)
		}
	}
}

// Regression: member names were free-text only, so every model name had to be
// typed from memory. The unified picker offers model names and group call names
// in a single datalist, and infers the member type from the name instead of
// asking the user to choose model/group up front.
func TestPanelMemberNamePickers(t *testing.T) {
	html := string(panelHTML())
	if !strings.Contains(html, `<datalist id="dl-members">`) {
		t.Error(`panel.html must declare the unified <datalist id="dl-members">`)
	}
	for _, stale := range []string{"dl-models", "dl-groups"} {
		if strings.Contains(html, stale) {
			t.Errorf("panel.html still declares the split picker %q", stale)
		}
	}
	if strings.Contains(html, "m-type") {
		t.Error("panel.html still renders the model/group type dropdown")
	}
	if !strings.Contains(html, `setAttribute("list","dl-members")`) {
		t.Error("the member name input must be bound to the unified datalist")
	}
	if !strings.Contains(html, "memberTypeOf(") {
		t.Error("panel.html must infer a member's type from its name")
	}
	if !strings.Contains(html, "memberPayload(") {
		t.Error("panel.html must build the member body from the inferred type")
	}
	if !strings.Contains(html, `api("/model-prices/runtime-models","GET")`) {
		t.Error("panel.html must load model names from the host runtime catalog")
	}
	if !strings.Contains(html, `api("/config","GET")`) {
		t.Error("panel.html must fall back to the config when the catalog is unavailable")
	}
	if !strings.Contains(html, "refreshDatalists()") {
		t.Error("panel.html must refresh the datalist after loading")
	}
}

// The unified picker must be able to emit either body shape: a name that matches
// a group candidate (call name, prefixed alias or internal name) becomes
// {group:...}; anything else becomes {model:...}.
func TestPanelHasUnifiedMemberPicker(t *testing.T) {
	html := string(panelHTML())
	if !strings.Contains(html, "memberPayload(") {
		t.Fatal("panel.html must build member bodies through memberPayload()")
	}
	if !strings.Contains(html, "{group:name,enabled:enabled}") {
		t.Error("memberPayload must emit {group:...} for group names")
	}
	if !strings.Contains(html, "{model:name,enabled:enabled}") {
		t.Error("memberPayload must emit {model:...} for model names")
	}
	if !strings.Contains(html, "collectGroupPicks(") {
		t.Error("panel.html must collect group call names / aliases / internal names")
	}
}

// Regression: a fallback group tries members in declaration order, so the edit
// modal must let that order be changed by dragging.
func TestPanelMemberRowsAreDraggable(t *testing.T) {
	html := string(panelHTML())
	if !strings.Contains(html, "handle.draggable=true") {
		t.Error("member rows must expose a draggable handle")
	}
	if !strings.Contains(html, `handle.className="drag"`) {
		t.Error("the drag handle must be identifiable")
	}
	if !strings.Contains(html, "mc.insertBefore(dragRow") {
		t.Error("dragging must actually reorder the member rows")
	}
	// The handle sits first in the row, so positional reads would pick it up
	// instead of the fields.
	if !strings.Contains(html, `r.querySelector(".m-name")`) {
		t.Error("saving must read member fields by class, not by child index")
	}
	if strings.Contains(html, "r.children[0].value") || strings.Contains(html, "r.children[1].value") {
		t.Error("member saving still uses fragile child indices")
	}
}

// The test panel must show the per-attempt trace, not a raw JSON dump.
func TestPanelRendersAttemptTrace(t *testing.T) {
	html := string(panelHTML())
	if !strings.Contains(html, "function renderTestResult(j)") {
		t.Error("panel.html must render the attempt trace")
	}
	if !strings.Contains(html, "renderTestResult(j);") {
		t.Error("the test button must use the trace renderer")
	}
	if strings.Contains(html, "out.textContent=JSON.stringify(j,null,2)") {
		t.Error("panel.html still dumps raw JSON for test results")
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"
)

type dashboardRow struct {
	Model       string
	App         string
	Requests    uint64
	TokensIn    uint64
	TokensOut   uint64
	Chars       uint64
	CostUSD     float64
	AvgLatMs    float64
	Errors      uint64
	Retries     uint64
	Fallbacks   uint64
	CacheHits   uint64
	CacheMisses uint64
}

type dashboardData struct {
	Rows             []dashboardRow
	Recent           []requestRecord
	TotalRequests    uint64
	TotalTokensIn    uint64
	TotalTokensOut   uint64
	TotalChars       uint64
	TotalCostUSD     float64
	TotalCacheHits   uint64
	TotalCacheMisses uint64
	CacheHitRate     string
	Uptime           string
}

// dashboardData builds a snapshot of the metrics for rendering. Called with the
// metrics mutex already held.
func (m *Metrics) dashboardData() dashboardData {
	maSet := map[string]bool{}
	for k := range m.tokensIn {
		maSet[k] = true
	}
	for k := range m.latency {
		maSet[k] = true
	}
	for k := range m.chars {
		maSet[k] = true
	}

	d := dashboardData{Rows: make([]dashboardRow, 0, len(maSet))}
	for k := range maSet {
		p := strings.SplitN(k, "|", 2)
		model, app := p[0], p[1]

		var reqs, errs uint64
		for rk, v := range m.requests {
			if strings.HasPrefix(rk, k+"|") {
				reqs += v
			}
		}
		for ek, v := range m.errors {
			if strings.HasPrefix(ek, k+"|") {
				errs += v
			}
		}
		var avgMs float64
		if h := m.latency[k]; h != nil && h.count > 0 {
			avgMs = h.sum / float64(h.count) * 1000
		}

		d.Rows = append(d.Rows, dashboardRow{
			Model:       model,
			App:         app,
			Requests:    reqs,
			TokensIn:    m.tokensIn[k],
			TokensOut:   m.tokensOut[k],
			Chars:       m.chars[k],
			CostUSD:     m.costUSD[k],
			Errors:      errs,
			AvgLatMs:    avgMs,
			Retries:     m.retries[k],
			Fallbacks:   m.fallbacks[k],
			CacheHits:   m.cacheHits[k],
			CacheMisses: m.cacheMisses[k],
		})
		d.TotalRequests += reqs
		d.TotalTokensIn += m.tokensIn[k]
		d.TotalTokensOut += m.tokensOut[k]
		d.TotalChars += m.chars[k]
		d.TotalCostUSD += m.costUSD[k]
		d.TotalCacheHits += m.cacheHits[k]
		d.TotalCacheMisses += m.cacheMisses[k]
	}
	sort.Slice(d.Rows, func(i, j int) bool { return d.Rows[i].CostUSD > d.Rows[j].CostUSD })

	if total := d.TotalCacheHits + d.TotalCacheMisses; total > 0 {
		d.CacheHitRate = fmt.Sprintf("%.0f%%", float64(d.TotalCacheHits)/float64(total)*100)
	} else {
		d.CacheHitRate = "—"
	}

	d.Recent = make([]requestRecord, len(m.recent))
	for i := range m.recent {
		d.Recent[len(m.recent)-1-i] = m.recent[i]
	}
	d.Uptime = time.Since(m.start).Round(time.Second).String()
	return d
}

// DashboardJSON serves the same snapshot as /dashboard, for the page's live
// updater and for anything that wants the numbers without the HTML.
func (m *Metrics) DashboardJSON() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		d := m.dashboardData()
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(d)
	})
}

// dashboardPage is the shell the browser receives. The numbers are not baked
// into the markup: the first snapshot ships alongside it as JSON and the page
// draws it. That leaves one renderer — the same one that handles every later
// poll — instead of two that have to be kept in step.
type dashboardPage struct {
	// template.JS, not template.HTML: inside a <script> element html/template
	// applies JS-context escaping, and template.HTML would be quoted into a
	// string literal rather than emitted as the object it already is.
	Boot template.JS
}

// Dashboard serves the live HTML metrics dashboard.
func (m *Metrics) Dashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		d := m.dashboardData()
		m.mu.Unlock()

		// json.Marshal escapes <, > and & by default, so the payload cannot
		// break out of the script element it is embedded in.
		boot, err := json.Marshal(d)
		if err != nil {
			boot = []byte("{}")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboardTmpl.Execute(w, dashboardPage{Boot: template.JS(boot)})
	})
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bifrost</title>
<style>
body{background:#0a0e1a;color:#c8d6e5;font-family:system-ui,-apple-system,sans-serif;margin:2rem auto;max-width:1000px;padding:0 1rem;-webkit-text-size-adjust:100%}
h1{color:#7fd4ff;font-weight:600}
h2{color:#9fb8cc;font-size:1rem;margin-top:2rem;text-transform:uppercase;letter-spacing:.05em}
.sub{color:#6b7a8a;font-weight:400;font-size:1rem;margin-left:.5rem}
.totals{display:flex;gap:1rem;flex-wrap:wrap;margin:1.5rem 0}
.total{border:1px solid #1e2a3a;border-radius:8px;padding:1rem 1.4rem;min-width:120px}
.total .v{font-size:1.6rem;color:#7fd4ff;font-family:ui-monospace,monospace}
.total .l{color:#6b7a8a;font-size:.78rem;margin-top:.2rem}
table{border-collapse:collapse;width:100%;margin:.75rem 0 1.5rem;font-size:.9rem}
th,td{border-bottom:1px solid #1e2a3a;padding:.45rem .6rem;text-align:right}
th:first-child,td:first-child{text-align:left}
th{color:#7fd4ff;font-weight:600}
.mono{font-family:ui-monospace,monospace}
.ok{color:#3ddc84}
.err{color:#ff6b6b}
.foot{color:#6b7a8a;font-size:.78rem}
.dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:#3ddc84;margin-right:.4rem;vertical-align:middle;animation:pulse 2s ease-in-out infinite}
.dot.stale{background:#ff6b6b;animation:none}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:.35}}
.flash{animation:flash .9s ease-out}
@keyframes flash{from{background:#16324a}to{background:transparent}}
/* Phones: totals become a 2-up grid and both tables turn into labelled
   cards, so nothing scrolls sideways and no column gets clipped. */
@media (max-width:640px){
  body{margin:.75rem auto;padding:0 .65rem}
  h1{font-size:1.3rem}
  .sub{display:block;margin:.15rem 0 0;font-size:.78rem}
  h2{font-size:.82rem;margin-top:1.4rem}
  .totals{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:.5rem;margin:1rem 0}
  .total{padding:.6rem .7rem;min-width:0}
  .total .v{font-size:1.15rem;overflow-wrap:anywhere}
  .total .l{font-size:.68rem}
  table{display:block;font-size:.86rem}
  thead{display:none}
  tbody{display:block}
  tbody tr{display:block;width:100%;box-sizing:border-box;border:1px solid #1e2a3a;border-radius:8px;background:#0d1424;padding:.45rem .7rem .55rem;margin-bottom:.55rem}
  tbody td{display:flex;justify-content:space-between;align-items:baseline;gap:.75rem;border:0;padding:.16rem 0;text-align:right}
  tbody td::before{content:attr(data-label);color:#6b7a8a;font-size:.76rem;text-align:left;flex:0 0 auto}
  tbody td:first-child{border-bottom:1px solid #1e2a3a;margin-bottom:.35rem;padding-bottom:.4rem;font-weight:600;font-size:.95rem;color:#7fd4ff;text-align:left;justify-content:flex-start;overflow-wrap:anywhere}
  tbody td:first-child::before{content:"";display:none}
  tbody td[colspan]{justify-content:center;text-align:center}
  tbody td[colspan]::before{content:"";display:none}
  .foot{font-size:.72rem}
}
a{color:#7fd4ff}
</style>
</head>
<body>
<h1>Bifrost<span class="sub">LLM gateway</span></h1>
<div class="totals">
  <div class="total"><div class="v" id="v-cost">—</div><div class="l">total cost (USD)</div></div>
  <div class="total"><div class="v" id="v-req">—</div><div class="l">requests</div></div>
  <div class="total"><div class="v" id="v-tin">—</div><div class="l">tokens in</div></div>
  <div class="total"><div class="v" id="v-tout">—</div><div class="l">tokens out</div></div>
  <div class="total"><div class="v" id="v-chars">—</div><div class="l">chars spoken</div></div>
  <div class="total"><div class="v" id="v-hits">—</div><div class="l">cache hits</div></div>
  <div class="total"><div class="v" id="v-rate">—</div><div class="l">cache hit rate</div></div>
  <div class="total"><div class="v" id="v-up">—</div><div class="l">uptime</div></div>
</div>

<h2>By model / app</h2>
<table>
<thead><tr><th>model</th><th>app</th><th>requests</th><th>tokens in</th><th>tokens out</th><th>chars</th><th>cost</th><th>avg latency</th><th>errors</th><th>retries</th><th>fallbacks</th><th>cache h/m</th></tr></thead>
<tbody id="rows"></tbody>
</table>

<h2>Recent requests</h2>
<table>
<thead><tr><th>time (UTC)</th><th>app</th><th>model</th><th>endpoint</th><th>status</th><th>tok in</th><th>tok out</th><th>cost</th><th>latency</th></tr></thead>
<tbody id="recent"></tbody>
</table>

<p class="foot"><span id="live" class="dot"></span><span id="status">connecting…</span></p>
<p class="foot">Prometheus format at <a href="/metrics">/metrics</a> · JSON at <a href="/dashboard.json">/dashboard.json</a></p>
<noscript><p class="foot err">This page draws its numbers with JavaScript. The same data is at <a href="/dashboard.json">/dashboard.json</a> and <a href="/metrics">/metrics</a>.</p></noscript>

<script type="application/json" id="boot">{{.Boot}}</script>

<script>
(function () {
  var REFRESH_MS = 2000;
  var lastCost = null;

  function esc(v) {
    return String(v).replace(/[&<>"']/g, function (c) {
      return {"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c];
    });
  }
  function num(n) { return Number(n || 0).toLocaleString(); }
  function money(n) { return "$" + Number(n || 0).toFixed(6); }
  function clock(t) {
    var d = new Date(t);
    return isNaN(d) ? "" : d.toISOString().slice(11, 19);
  }

  function rowsHTML(rows) {
    if (!rows || !rows.length) return '<tr><td colspan="12" class="foot">no traffic yet</td></tr>';
    return rows.map(function (r) {
      return '<tr>' +
        '<td class="mono" data-label="model">' + esc(r.Model) + '</td>' +
        '<td data-label="app">' + esc(r.App) + '</td>' +
        '<td data-label="requests">' + num(r.Requests) + '</td>' +
        '<td data-label="tokens in">' + num(r.TokensIn) + '</td>' +
        '<td data-label="tokens out">' + num(r.TokensOut) + '</td>' +
        '<td data-label="chars spoken">' + num(r.Chars) + '</td>' +
        '<td class="mono" data-label="cost">' + money(r.CostUSD) + '</td>' +
        '<td class="mono" data-label="avg latency">' + Math.round(r.AvgLatMs) + ' ms</td>' +
        '<td class="' + (r.Errors ? 'err' : '') + '" data-label="errors">' + num(r.Errors) + '</td>' +
        '<td data-label="retries">' + num(r.Retries) + '</td>' +
        '<td data-label="fallbacks">' + num(r.Fallbacks) + '</td>' +
        '<td data-label="cache hits/miss">' + num(r.CacheHits) + '/' + num(r.CacheMisses) + '</td>' +
        '</tr>';
    }).join('');
  }

  function recentHTML(rows) {
    if (!rows || !rows.length) return '<tr><td colspan="9" class="foot">no requests yet</td></tr>';
    return rows.slice(0, 60).map(function (r) {
      return '<tr>' +
        '<td class="mono" data-label="time">' + clock(r.Time) + '</td>' +
        '<td data-label="app">' + esc(r.App) + '</td>' +
        '<td class="mono" data-label="model">' + esc(r.Model) + '</td>' +
        '<td class="mono" data-label="endpoint">' + esc(r.Endpoint) + '</td>' +
        '<td class="' + (r.Err ? 'err' : 'ok') + '" data-label="status">' + r.Status + '</td>' +
        '<td data-label="tokens in">' + num(r.TokensIn) + '</td>' +
        '<td data-label="tokens out">' + num(r.TokensOut) + '</td>' +
        '<td class="mono" data-label="cost">' + money(r.CostUSD) + '</td>' +
        '<td class="mono" data-label="latency">' + Math.round(r.DurationMs) + ' ms</td>' +
        '</tr>';
    }).join('');
  }

  function setText(id, v) {
    var el = document.getElementById(id);
    if (el && el.textContent !== String(v)) el.textContent = v;
  }

  function apply(d) {
    setText("v-cost", money(d.TotalCostUSD));
    setText("v-req", num(d.TotalRequests));
    setText("v-tin", num(d.TotalTokensIn));
    setText("v-tout", num(d.TotalTokensOut));
    setText("v-chars", num(d.TotalChars));
    setText("v-hits", num(d.TotalCacheHits));
    setText("v-rate", d.CacheHitRate);
    setText("v-up", d.Uptime);

    document.getElementById("rows").innerHTML = rowsHTML(d.Rows);
    document.getElementById("recent").innerHTML = recentHTML(d.Recent);

    // Flash the cost card only when it actually moved, so a quiet gateway
    // stays still and a busy one is visibly alive.
    var cost = Number(d.TotalCostUSD || 0).toFixed(6);
    if (lastCost !== null && cost !== lastCost) {
      var el = document.getElementById("v-cost");
      el.classList.remove("flash");
      void el.offsetWidth;
      el.classList.add("flash");
    }
    lastCost = cost;
  }

  function markLive(ok, msg) {
    var s = document.getElementById("status"), dot = document.getElementById("live");
    s.textContent = msg;
    s.className = ok ? "foot" : "foot err";
    dot.className = ok ? "dot" : "dot stale";
  }

  function tick() {
    fetch("/dashboard.json", {cache: "no-store"})
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (d) {
        apply(d);
        markLive(true, "live · updated " + new Date().toLocaleTimeString());
      })
      .catch(function (e) {
        markLive(false, "disconnected (" + e.message + ") · retrying");
      });
  }

  // The first snapshot ships inside the page, so the first paint costs no
  // round trip. The poll below only has to keep it current.
  try {
    apply(JSON.parse(document.getElementById("boot").textContent));
    markLive(true, "live · updated " + new Date().toLocaleTimeString());
  } catch (e) {
    markLive(false, "no initial snapshot (" + e.message + ")");
  }

  tick();
  setInterval(function () { if (!document.hidden) tick(); }, REFRESH_MS);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) tick(); });
})();
</script>
</body>
</html>
`))

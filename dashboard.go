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

// Dashboard serves the live HTML metrics dashboard.
func (m *Metrics) Dashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboardTmpl.Execute(w, m.dashboardData())
	})
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Bifrost</title>
<style>
body{background:#0a0e1a;color:#c8d6e5;font-family:system-ui,-apple-system,sans-serif;margin:2rem auto;max-width:1000px;padding:0 1rem}
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
a{color:#7fd4ff}
</style>
</head>
<body>
<h1>Bifrost<span class="sub">LLM gateway</span></h1>
<div class="totals">
  <div class="total"><div class="v" id="v-cost">${{printf "%.6f" .TotalCostUSD}}</div><div class="l">total cost (USD)</div></div>
  <div class="total"><div class="v" id="v-req">{{.TotalRequests}}</div><div class="l">requests</div></div>
  <div class="total"><div class="v" id="v-tin">{{.TotalTokensIn}}</div><div class="l">tokens in</div></div>
  <div class="total"><div class="v" id="v-tout">{{.TotalTokensOut}}</div><div class="l">tokens out</div></div>
  <div class="total"><div class="v" id="v-chars">{{.TotalChars}}</div><div class="l">chars spoken</div></div>
  <div class="total"><div class="v" id="v-hits">{{.TotalCacheHits}}</div><div class="l">cache hits</div></div>
  <div class="total"><div class="v" id="v-rate">{{.CacheHitRate}}</div><div class="l">cache hit rate</div></div>
  <div class="total"><div class="v" id="v-up">{{.Uptime}}</div><div class="l">uptime</div></div>
</div>

<h2>By model / app</h2>
<table>
<tr><th>model</th><th>app</th><th>requests</th><th>tokens in</th><th>tokens out</th><th>chars</th><th>cost</th><th>avg latency</th><th>errors</th><th>retries</th><th>fallbacks</th><th>cache h/m</th></tr>
<tbody id="rows">
{{range .Rows}}<tr>
<td class="mono">{{.Model}}</td><td>{{.App}}</td><td>{{.Requests}}</td><td>{{.TokensIn}}</td><td>{{.TokensOut}}</td><td>{{.Chars}}</td>
<td class="mono">${{printf "%.6f" .CostUSD}}</td><td class="mono">{{printf "%.0f" .AvgLatMs}} ms</td>
<td class="{{if .Errors}}err{{end}}">{{.Errors}}</td>
<td>{{.Retries}}</td><td>{{.Fallbacks}}</td><td>{{.CacheHits}}/{{.CacheMisses}}</td>
</tr>{{end}}
</tbody>
</table>

<h2>Recent requests</h2>
<table>
<tr><th>time (UTC)</th><th>app</th><th>model</th><th>endpoint</th><th>status</th><th>tok in</th><th>tok out</th><th>cost</th><th>latency</th></tr>
<tbody id="recent">
{{range .Recent}}<tr>
<td class="mono">{{.Time.Format "15:04:05"}}</td><td>{{.App}}</td><td class="mono">{{.Model}}</td><td class="mono">{{.Endpoint}}</td>
<td class="{{if .Err}}err{{else}}ok{{end}}">{{.Status}}</td>
<td>{{.TokensIn}}</td><td>{{.TokensOut}}</td><td class="mono">${{printf "%.6f" .CostUSD}}</td><td class="mono">{{printf "%.0f" .DurationMs}} ms</td>
</tr>{{end}}
</tbody>
</table>

<p class="foot"><span id="live" class="dot"></span><span id="status">connecting…</span></p>
<p class="foot">Prometheus format at <a href="/metrics">/metrics</a> · JSON at <a href="/dashboard.json">/dashboard.json</a></p>

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
        '<td class="mono">' + esc(r.Model) + '</td>' +
        '<td>' + esc(r.App) + '</td>' +
        '<td>' + num(r.Requests) + '</td>' +
        '<td>' + num(r.TokensIn) + '</td>' +
        '<td>' + num(r.TokensOut) + '</td>' +
        '<td>' + num(r.Chars) + '</td>' +
        '<td class="mono">' + money(r.CostUSD) + '</td>' +
        '<td class="mono">' + Math.round(r.AvgLatMs) + ' ms</td>' +
        '<td class="' + (r.Errors ? 'err' : '') + '">' + num(r.Errors) + '</td>' +
        '<td>' + num(r.Retries) + '</td>' +
        '<td>' + num(r.Fallbacks) + '</td>' +
        '<td>' + num(r.CacheHits) + '/' + num(r.CacheMisses) + '</td>' +
        '</tr>';
    }).join('');
  }

  function recentHTML(rows) {
    if (!rows || !rows.length) return '<tr><td colspan="9" class="foot">no requests yet</td></tr>';
    return rows.slice(0, 60).map(function (r) {
      return '<tr>' +
        '<td class="mono">' + clock(r.Time) + '</td>' +
        '<td>' + esc(r.App) + '</td>' +
        '<td class="mono">' + esc(r.Model) + '</td>' +
        '<td class="mono">' + esc(r.Endpoint) + '</td>' +
        '<td class="' + (r.Err ? 'err' : 'ok') + '">' + r.Status + '</td>' +
        '<td>' + num(r.TokensIn) + '</td>' +
        '<td>' + num(r.TokensOut) + '</td>' +
        '<td class="mono">' + money(r.CostUSD) + '</td>' +
        '<td class="mono">' + Math.round(r.DurationMs) + ' ms</td>' +
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

  tick();
  setInterval(function () { if (!document.hidden) tick(); }, REFRESH_MS);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) tick(); });
})();
</script>
</body>
</html>
`))
